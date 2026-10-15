package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ActivityPub client-to-server: a local user's client POSTs activities to
// their outbox (authorized with an OAuth bearer token from /oauth/token,
// scope "write"), and reads their inbox (scope "read"). Activities are
// mapped onto the same actions the web UI and the Mastodon API use, so
// federation, notifications and permissions work the same way.

const maxC2SBody = 1 << 20

var errC2SUnsupported = errors.New("unsupported activity")

// c2sUser returns the outbox/inbox owner if the request's token belongs to
// them and has the scope.
func (app *App) c2sUser(w http.ResponseWriter, r *http.Request, scope string) *Account {
	owner, err := app.LocalAccount(r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	c := app.apiAuth(r)
	switch {
	case c == nil || c.Me == nil:
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+app.cfg.Host+`"`)
		http.Error(w, "this needs an OAuth bearer token of the account", http.StatusUnauthorized)
	case c.Me.ID != owner.ID:
		http.Error(w, "not your outbox", http.StatusForbidden)
	case !c.can(scope):
		http.Error(w, "token lacks the "+scope+" scope", http.StatusForbidden)
	default:
		return c.Me
	}
	return nil
}

func (app *App) handleC2SOutbox(w http.ResponseWriter, r *http.Request) {
	me := app.c2sUser(w, r, "write")
	if me == nil {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxC2SBody+1))
	if err != nil || len(body) > maxC2SBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var act obj
	if err := json.Unmarshal(body, &act); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	loc, err := app.c2sActivity(r.Context(), me, act)
	switch {
	case errors.Is(err, errNotFound):
		http.Error(w, "object not found", http.StatusNotFound)
	case errors.Is(err, errC2SUnsupported):
		http.Error(w, err.Error()+": "+typeOf(act), http.StatusUnprocessableEntity)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusCreated)
	}
}

// c2sActivity performs one client activity and returns the id of the
// activity that was created.
func (app *App) c2sActivity(ctx context.Context, me *Account, act obj) (string, error) {
	switch typeOf(act) {
	case "Note", "Article", "Question":
		// a bare object is wrapped in a Create (AP §6.2.1)
		return app.c2sCreate(ctx, me, act, act)
	case "Create":
		return app.c2sCreate(ctx, me, act, objOf(act["object"]))
	case "Update":
		return app.c2sUpdate(ctx, me, objOf(act["object"]))
	case "Delete":
		st, err := app.c2sOwnStatus(me, idOf(act["object"]))
		if err != nil {
			return "", err
		}
		return st.URI + "#delete", app.DeleteStatus(me, st)
	case "Like", "Announce":
		st, err := app.c2sStatus(ctx, me, idOf(act["object"]))
		if err != nil {
			return "", err
		}
		if typeOf(act) == "Like" {
			if err := app.Like(me, st); err != nil {
				return "", err
			}
			var uri string
			app.db.QueryRow(`SELECT uri FROM likes WHERE account_id=? AND status_id=?`, me.ID, st.ID).Scan(&uri)
			return uri, nil
		}
		if err := app.Boost(me, st); err != nil {
			return "", err
		}
		var uri string
		app.db.QueryRow(`SELECT uri FROM statuses WHERE account_id=? AND boost_of_id=?`, me.ID, st.ID).Scan(&uri)
		return uri, nil
	case "Follow", "Block":
		target, err := app.getAccount(ctx, idOf(act["object"]))
		if err != nil {
			return "", errNotFound
		}
		if target.ID == me.ID {
			return "", errors.New("that's you")
		}
		if typeOf(act) == "Block" {
			return me.URI + "#blocks/" + target.ID, app.Block(me, target)
		}
		if err := app.Follow(me, target); err != nil {
			return "", err
		}
		var uri string
		app.db.QueryRow(`SELECT uri FROM follows WHERE account_id=? AND target_id=?`, me.ID, target.ID).Scan(&uri)
		return uri, nil
	case "Undo":
		return app.c2sUndo(ctx, me, act["object"])
	}
	return "", errC2SUnsupported
}

func (app *App) c2sCreate(ctx context.Context, me *Account, act, note obj) (string, error) {
	switch typeOf(note) {
	case "Note", "Article":
	default:
		return "", errC2SUnsupported
	}
	// addressing may be on the activity, the object or both
	addr := obj{}
	for _, k := range []string{"to", "cc"} {
		var ids []any
		for _, id := range append(idsOf(act[k]), idsOf(note[k])...) {
			ids = append(ids, id)
		}
		addr[k] = ids
	}
	in := PostInput{
		Text:       c2sText(note),
		Spoiler:    str(note, "summary"),
		Visibility: c2sVisibility(addr, me),
	}
	in.Sensitive, _ = note["sensitive"].(bool)
	if irt := idOf(note["inReplyTo"]); irt != "" {
		parent, err := app.c2sStatus(ctx, me, irt)
		if err != nil {
			return "", err
		}
		in.InReplyTo = parent.ID
	}
	in.Text = app.c2sMentions(ctx, me, in.Text, addr, note)
	st, err := app.PostStatus(ctx, me, in)
	if err != nil {
		return "", err
	}
	return st.URI + "/activity", nil
}

func (app *App) c2sUpdate(ctx context.Context, me *Account, note obj) (string, error) {
	switch typeOf(note) {
	case "Note", "Article":
	default:
		return "", errC2SUnsupported
	}
	st, err := app.c2sOwnStatus(me, idOf(note))
	if err != nil {
		return "", err
	}
	sensitive, _ := note["sensitive"].(bool)
	st, err = app.EditStatus(ctx, me, st, c2sText(note), str(note, "summary"), sensitive)
	if err != nil {
		return "", err
	}
	return st.URI + "#updates/" + newID(), nil
}

func (app *App) c2sUndo(ctx context.Context, me *Account, o any) (string, error) {
	id := idOf(o)
	inner := objOf(o)
	typ, target := typeOf(inner), idOf(inner["object"])
	if typ == "" || target == "" {
		// a bare id: one of our likes, boosts or follows
		var x string
		switch {
		case app.db.QueryRow(`SELECT s.uri FROM likes l JOIN statuses s ON s.id=l.status_id WHERE l.uri=? AND l.account_id=?`, id, me.ID).Scan(&x) == nil:
			typ = "Like"
		case app.db.QueryRow(`SELECT o.uri FROM statuses b JOIN statuses o ON o.id=b.boost_of_id WHERE b.uri=? AND b.account_id=?`, id, me.ID).Scan(&x) == nil:
			typ = "Announce"
		case app.db.QueryRow(`SELECT a.uri FROM follows f JOIN accounts a ON a.id=f.target_id WHERE f.uri=? AND f.account_id=?`, id, me.ID).Scan(&x) == nil:
			typ = "Follow"
		default:
			return "", errNotFound
		}
		target = x
	}
	undoID := me.URI + "#undo/" + newID()
	switch typ {
	case "Like", "Announce":
		st, err := app.c2sStatus(ctx, me, target)
		if err != nil {
			return "", err
		}
		if typ == "Like" {
			return undoID, app.Unlike(me, st)
		}
		return undoID, app.Unboost(me, st)
	case "Follow", "Block":
		acc, err := app.AccountByURI(target)
		if err != nil {
			return "", errNotFound
		}
		if typ == "Block" {
			return undoID, app.Unblock(me, acc)
		}
		return undoID, app.Unfollow(me, acc)
	}
	return "", errC2SUnsupported
}

// c2sStatus finds a status the user can see, fetching remote ones.
func (app *App) c2sStatus(ctx context.Context, me *Account, iri string) (*Status, error) {
	if iri == "" {
		return nil, errNotFound
	}
	st, err := app.resolveStatus(ctx, iri)
	if err != nil {
		return nil, errNotFound
	}
	st, err = app.loadStatus(me, st.ID)
	if err != nil {
		return nil, errNotFound
	}
	return st, nil
}

func (app *App) c2sOwnStatus(me *Account, iri string) (*Status, error) {
	st, err := app.StatusByURI(iri)
	if err != nil || st.AccountID != me.ID || st.IsBoost() {
		return nil, errNotFound
	}
	return app.loadStatus(me, st.ID)
}

// c2sText prefers the plain source the client wrote over its HTML.
func c2sText(note obj) string {
	if src := objOf(note["source"]); src != nil {
		if mt := str(src, "mediaType"); mt == "" || strings.HasPrefix(mt, "text/plain") || strings.HasPrefix(mt, "text/markdown") {
			return str(src, "content")
		}
	}
	c := str(note, "content")
	if strings.ContainsAny(c, "<>") {
		return plainText(c)
	}
	return c
}

func c2sVisibility(addr obj, me *Account) string {
	has := func(k, want string) bool {
		for _, id := range idsOf(addr[k]) {
			if id == want || (want == asPublic && (id == "as:Public" || id == "Public")) {
				return true
			}
		}
		return false
	}
	switch {
	case has("to", asPublic):
		return "public"
	case has("cc", asPublic):
		return "unlisted"
	case has("to", me.FollowersURI) || has("cc", me.FollowersURI):
		return "private"
	}
	return "direct"
}

// c2sMentions makes sure everyone the client addressed or tagged is
// mentioned in the text, since that's how we pick recipients.
func (app *App) c2sMentions(ctx context.Context, me *Account, text string, addr, note obj) string {
	var iris []string
	for _, k := range []string{"to", "cc"} {
		iris = append(iris, idsOf(addr[k])...)
	}
	for _, t := range objsOf(note["tag"]) {
		if typeOf(t) == "Mention" {
			iris = append(iris, str(t, "href"))
		}
	}
	seen := map[string]bool{}
	var missing []string
	for _, iri := range iris {
		if iri == "" || seen[iri] || iri == asPublic || iri == me.FollowersURI || strings.HasSuffix(iri, "/followers") {
			continue
		}
		seen[iri] = true
		if u, err := url.Parse(iri); err != nil || u.Host == "" {
			continue
		}
		acc, err := app.getAccount(ctx, iri)
		if err != nil || acc.ID == me.ID {
			continue
		}
		handle := "@" + acc.Acct()
		if !strings.Contains(strings.ToLower(text), strings.ToLower(handle)) {
			missing = append(missing, handle)
		}
	}
	if len(missing) == 0 {
		return text
	}
	return strings.Join(missing, " ") + " " + text
}

// handleC2SInbox shows the owner their home timeline as activities.
func (app *App) handleC2SInbox(w http.ResponseWriter, r *http.Request) {
	me := app.c2sUser(w, r, "read")
	if me == nil {
		return
	}
	id := me.Inbox
	if r.URL.Query().Get("page") == "" {
		writeAP(w, withContext(obj{"id": id, "type": "OrderedCollection", "first": id + "?page=true"}))
		return
	}
	maxID := r.URL.Query().Get("max_id")
	sts, err := app.homeTimeline(me, maxID)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	items := []any{}
	for _, st := range sts {
		if st.IsBoost() {
			items = append(items, delContext(app.announceActivity(st)))
		} else {
			items = append(items, delContext(app.createActivity(st)))
		}
	}
	page := obj{"id": id + "?page=true", "type": "OrderedCollectionPage", "partOf": id, "orderedItems": items}
	if maxID != "" {
		page["id"] = id + "?page=true&max_id=" + url.QueryEscape(maxID)
	}
	if len(sts) > 0 {
		page["next"] = id + "?page=true&max_id=" + sts[len(sts)-1].ID
	}
	writeAP(w, withContext(page))
}
