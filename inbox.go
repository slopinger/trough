package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type inboxJob struct {
	signer   *Account
	activity obj
}

// handleInbox serves POST /inbox and /users/{name}/inbox.
func (app *App) handleInbox(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONSize+1))
	if err != nil || len(body) > maxJSONSize {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var act obj
	if err := jsonUnmarshal(body, &act); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	actorIRI := idOf(act["actor"])
	if actorIRI == "" {
		http.Error(w, "activity without actor", http.StatusBadRequest)
		return
	}
	if app.domainBlocked(hostOf(actorIRI)) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	// Deletes of accounts we never heard of are very common (servers
	// broadcast them everywhere); don't bother fetching a key that is gone.
	if typeOf(act) == "Delete" && idOf(act["object"]) == actorIRI {
		if _, err := app.AccountByURI(actorIRI); err != nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}
	signer, err := app.verifyRequest(r, body)
	if err != nil {
		if errors.Is(err, errBlocked) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		log.Printf("inbox: rejecting %s %s from %s: %v", typeOf(act), str(act, "id"), actorIRI, err)
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}
	if signer.URI != stripFragment(actorIRI) {
		log.Printf("inbox: signer %s does not match actor %s", signer.URI, actorIRI)
		http.Error(w, "signer does not match actor", http.StatusUnauthorized)
		return
	}
	if signer.Suspended {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	select {
	case app.inboxQueue <- inboxJob{signer, act}:
		w.WriteHeader(http.StatusAccepted)
	default:
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}
}

func (app *App) inboxWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-app.inboxQueue:
			jctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			if err := app.processActivity(jctx, job.signer, job.activity); err != nil {
				log.Printf("inbox: %s %s from %s: %v", typeOf(job.activity), str(job.activity, "id"), job.signer.URI, err)
			}
			cancel()
		}
	}
}

func (app *App) isLocalIRI(iri string) bool {
	return strings.HasPrefix(iri, app.cfg.BaseURL()+"/")
}

func (app *App) processActivity(ctx context.Context, actor *Account, act obj) error {
	switch typeOf(act) {
	case "Follow":
		return app.inFollow(actor, act)
	case "Accept", "Reject":
		return app.inAcceptReject(ctx, actor, act)
	case "Undo":
		return app.inUndo(actor, act)
	case "Create":
		return app.inCreate(ctx, actor, act)
	case "Update":
		return app.inUpdate(ctx, actor, act)
	case "Delete":
		return app.inDelete(actor, act)
	case "Like":
		return app.inLike(actor, act)
	case "Announce":
		return app.inAnnounce(ctx, actor, act)
	case "Block":
		return app.inBlock(actor, act)
	}
	return nil
}

func (app *App) localTarget(iri string) (*Account, error) {
	if !app.isLocalIRI(iri) {
		return nil, errNotFound
	}
	acc, err := app.AccountByURI(iri)
	if err != nil || acc.ActorType == "Application" {
		return nil, errNotFound
	}
	return acc, nil
}

func (app *App) inFollow(actor *Account, act obj) error {
	target, err := app.localTarget(idOf(act["object"]))
	if err != nil {
		return err
	}
	followURI := str(act, "id")
	if followURI == "" {
		return errors.New("follow without id")
	}
	if app.isBlockedBy(target.ID, actor.ID) {
		app.enqueue(target, []string{actor.Inbox}, acceptRejectActivity("Reject", target, actor, followURI))
		return nil
	}
	var accepted bool
	err = app.db.QueryRow(`SELECT accepted FROM follows WHERE account_id=? AND target_id=?`, actor.ID, target.ID).Scan(&accepted)
	if err == nil {
		// already known: update the uri (Mastodon re-follows with a new id) and re-accept
		app.db.Exec(`UPDATE follows SET uri=? WHERE account_id=? AND target_id=?`, followURI, actor.ID, target.ID)
		if accepted {
			app.enqueue(target, []string{actor.Inbox}, acceptRejectActivity("Accept", target, actor, followURI))
		}
		return nil
	}
	accepted = !target.Locked
	if _, err := app.db.Exec(`INSERT INTO follows (id,uri,account_id,target_id,accepted,created_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(uri) DO NOTHING`, newID(), followURI, actor.ID, target.ID, accepted, time.Now().Unix()); err != nil {
		return err
	}
	if accepted {
		app.notify(target.ID, "follow", actor.ID, "")
		app.enqueue(target, []string{actor.Inbox}, acceptRejectActivity("Accept", target, actor, followURI))
	} else {
		app.notify(target.ID, "follow_request", actor.ID, "")
	}
	return nil
}

func (app *App) inAcceptReject(ctx context.Context, actor *Account, act obj) error {
	o := act["object"]
	followURI := idOf(o)
	var followerID string
	err := app.db.QueryRow(`SELECT account_id FROM follows WHERE uri=? AND target_id=?`, followURI, actor.ID).Scan(&followerID)
	if err != nil {
		// some servers only embed actor/object of our Follow
		if m, ok := o.(obj); ok && typeOf(m) == "Follow" {
			if me, err := app.localTarget(idOf(m["actor"])); err == nil {
				followerID = me.ID
			}
		}
		if followerID == "" {
			return errors.New("unknown follow")
		}
	}
	if typeOf(act) == "Accept" {
		app.db.Exec(`UPDATE follows SET accepted=1 WHERE account_id=? AND target_id=?`, followerID, actor.ID)
		go app.backfillOutbox(context.Background(), actor)
	} else {
		app.db.Exec(`DELETE FROM follows WHERE account_id=? AND target_id=?`, followerID, actor.ID)
	}
	return nil
}

func (app *App) inUndo(actor *Account, act obj) error {
	o := act["object"]
	innerID := idOf(o)
	inner, _ := o.(obj)
	kind := ""
	if inner != nil {
		kind = typeOf(inner)
		if a := idOf(inner["actor"]); a != "" && a != actor.URI {
			return errors.New("undo of someone else's activity")
		}
	}
	switch kind {
	case "Follow", "":
		var targetID string
		err := app.db.QueryRow(`DELETE FROM follows WHERE uri=? AND account_id=? RETURNING target_id`, innerID, actor.ID).Scan(&targetID)
		if errors.Is(err, sql.ErrNoRows) && inner != nil {
			if target, targetErr := app.localTarget(idOf(inner["object"])); targetErr == nil {
				err = app.db.QueryRow(`DELETE FROM follows WHERE account_id=? AND target_id=? RETURNING target_id`, actor.ID, target.ID).Scan(&targetID)
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if _, err := app.db.Exec(`DELETE FROM notifications WHERE account_id=? AND from_account_id=? AND type IN ('follow','follow_request')`, targetID, actor.ID); err != nil {
				return err
			}
		}
		if kind == "Follow" {
			return nil
		}
		fallthrough
	case "Like":
		var statusID string
		err := app.db.QueryRow(`DELETE FROM likes WHERE uri=? AND account_id=? RETURNING status_id`, innerID, actor.ID).Scan(&statusID)
		if errors.Is(err, sql.ErrNoRows) && inner != nil {
			if st, statusErr := app.StatusByURI(idOf(inner["object"])); statusErr == nil {
				err = app.db.QueryRow(`DELETE FROM likes WHERE account_id=? AND status_id=? RETURNING status_id`, actor.ID, st.ID).Scan(&statusID)
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if _, err := app.db.Exec(`DELETE FROM notifications WHERE type='favourite' AND from_account_id=? AND status_id=?`, actor.ID, statusID); err != nil {
				return err
			}
		}
		if kind == "Like" {
			return nil
		}
		fallthrough
	case "Announce":
		if b, err := app.statusWhere(`s.uri=? AND s.account_id=? AND s.boost_of_id != ''`, innerID, actor.ID); err == nil {
			app.db.Exec(`DELETE FROM notifications WHERE type='reblog' AND from_account_id=? AND status_id=?`, actor.ID, b.BoostOfID)
			app.db.Exec(`DELETE FROM statuses WHERE id=?`, b.ID)
		}
		if kind == "Announce" {
			return nil
		}
		fallthrough
	case "Block":
		app.db.Exec(`DELETE FROM blocks WHERE uri=? AND account_id=?`, innerID, actor.ID)
	}
	return nil
}

func (app *App) inBlock(actor *Account, act obj) error {
	target, err := app.localTarget(idOf(act["object"]))
	if err != nil {
		return err
	}
	app.db.Exec(`INSERT OR IGNORE INTO blocks (id,uri,account_id,target_id,created_at) VALUES (?,?,?,?,?)`,
		newID(), str(act, "id"), actor.ID, target.ID, time.Now().Unix())
	app.db.Exec(`DELETE FROM follows WHERE (account_id=? AND target_id=?) OR (account_id=? AND target_id=?)`, actor.ID, target.ID, target.ID, actor.ID)
	return nil
}

func (app *App) inLike(actor *Account, act obj) error {
	st, err := app.StatusByURI(idOf(act["object"]))
	if err != nil {
		return nil // not something we know about
	}
	uri := str(act, "id")
	if uri == "" {
		uri = actor.URI + "#likes/" + st.ID
	}
	res, err := app.db.Exec(`INSERT OR IGNORE INTO likes (id,uri,account_id,status_id,created_at) VALUES (?,?,?,?,?)`,
		newID(), uri, actor.ID, st.ID, time.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 && st.Local {
		app.notify(st.AccountID, "favourite", actor.ID, st.ID)
	}
	return nil
}

func (app *App) inAnnounce(ctx context.Context, actor *Account, act obj) error {
	uri := str(act, "id")
	if uri == "" {
		return errors.New("announce without id")
	}
	if _, err := app.StatusByURI(uri); err == nil {
		return nil
	}
	objIRI := idOf(act["object"])
	orig, err := app.StatusByURI(objIRI)
	if err != nil {
		// only pull in boosted posts for accounts somebody here follows
		if !app.hasLocalFollowers(actor.ID) {
			return nil
		}
		orig, err = app.resolveStatus(ctx, objIRI)
		if err != nil {
			return err
		}
	}
	if orig.Visibility != "public" && orig.Visibility != "unlisted" {
		return nil
	}
	published := parseTime(str(act, "published"))
	if published.IsZero() || published.After(time.Now()) {
		published = time.Now()
	}
	b := &Status{
		ID: newIDAt(published), URI: uri, URL: uri, AccountID: actor.ID, BoostOfID: orig.ID,
		Visibility: visibilityFrom(act, actor.FollowersURI), CreatedAt: published,
	}
	if b.Visibility != "public" && b.Visibility != "unlisted" {
		b.Visibility = "unlisted"
	}
	if err := app.insertStatus(b); err != nil {
		return err
	}
	if orig.Local {
		app.notify(orig.AccountID, "reblog", actor.ID, orig.ID)
	}
	return nil
}

func (app *App) hasLocalFollowers(accountID string) bool {
	var x int
	return app.db.QueryRow(`SELECT 1 FROM follows f JOIN accounts a ON a.id=f.account_id WHERE f.target_id=? AND f.accepted=1 AND a.domain='' LIMIT 1`, accountID).Scan(&x) == nil
}

var noteTypes = map[string]bool{"Note": true, "Article": true, "Question": true, "Page": true, "Video": true, "Event": true, "Audio": true, "Image": true}

func (app *App) inCreate(ctx context.Context, actor *Account, act obj) error {
	o := act["object"]
	note, ok := o.(obj)
	if !ok {
		// only a reference; fetch it
		m, err := app.fetchAP(ctx, idOf(o))
		if err != nil {
			return err
		}
		note = m
	}
	if !noteTypes[typeOf(note)] {
		return nil
	}
	if idOf(note["attributedTo"]) != actor.URI || !sameHost(str(note, "id"), actor.URI) {
		// forwarded or forged: trust only what the origin server says
		m, err := app.fetchAP(ctx, str(note, "id"))
		if err != nil {
			return err
		}
		note = m
		author, err := app.getAccount(ctx, idOf(note["attributedTo"]))
		if err != nil {
			return err
		}
		actor = author
		if !sameHost(str(note, "id"), actor.URI) {
			return errors.New("note and author on different hosts")
		}
	}
	if !app.relevant(actor, note) {
		return nil
	}
	_, err := app.storeNote(ctx, actor, note, 0)
	return err
}

// relevant decides whether we want an incoming post: it is by someone a
// local user follows, addresses a local user, or replies to a local post.
func (app *App) relevant(author *Account, note obj) bool {
	if app.hasLocalFollowers(author.ID) {
		return true
	}
	for _, r := range recipientsOf(note) {
		if app.isLocalIRI(r) {
			return true
		}
	}
	for _, t := range objsOf(note["tag"]) {
		if typeOf(t) == "Mention" && app.isLocalIRI(str(t, "href")) {
			return true
		}
	}
	if p := idOf(note["inReplyTo"]); p != "" {
		if _, err := app.StatusByURI(p); err == nil {
			return true
		}
	}
	return false
}

func (app *App) inUpdate(ctx context.Context, actor *Account, act obj) error {
	o, _ := act["object"].(obj)
	if o == nil {
		return nil
	}
	if actorTypes[typeOf(o)] {
		if str(o, "id") != actor.URI {
			return errors.New("update of another actor")
		}
		_, err := app.upsertActor(o)
		return err
	}
	if noteTypes[typeOf(o)] {
		st, err := app.StatusByURI(str(o, "id"))
		if err != nil {
			return nil
		}
		if st.AccountID != actor.ID || idOf(o["attributedTo"]) != actor.URI {
			return errors.New("update of someone else's post")
		}
		app.applyNote(st, o)
		if err := app.updateStatusContent(st); err != nil {
			return err
		}
		app.replaceNoteExtras(ctx, st, o)
	}
	return nil
}

func (app *App) inDelete(actor *Account, act obj) error {
	iri := idOf(act["object"])
	if iri == actor.URI {
		// the account is gone; remove what we have of it
		log.Printf("inbox: account %s deleted", actor.URI)
		app.db.Exec(`DELETE FROM accounts WHERE id=?`, actor.ID)
		return nil
	}
	st, err := app.StatusByURI(iri)
	if err != nil {
		return nil
	}
	if st.AccountID != actor.ID {
		return errors.New("delete of someone else's post")
	}
	return app.purgeStatus(st.ID)
}
