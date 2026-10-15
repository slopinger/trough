package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- pagination ----

type pageQ struct {
	MaxID, SinceID, MinID string
	Limit                 int
}

func pageParams(r *http.Request, def, max int) pageQ {
	q := r.URL.Query()
	pq := pageQ{MaxID: q.Get("max_id"), SinceID: q.Get("since_id"), MinID: q.Get("min_id"), Limit: def}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		pq.Limit = min(n, max)
	}
	return pq
}

// pageStatuses runs a timeline query with Mastodon pagination semantics.
func (app *App) pageStatuses(v *Account, where string, args []any, pq pageQ) ([]*Status, error) {
	extra, eargs := "", []any{}
	if pq.MaxID != "" {
		extra += " AND s.id < ?"
		eargs = append(eargs, pq.MaxID)
	}
	if pq.SinceID != "" {
		extra += " AND s.id > ?"
		eargs = append(eargs, pq.SinceID)
	}
	asc := false
	if pq.MinID != "" {
		extra += " AND s.id > ?"
		eargs = append(eargs, pq.MinID)
		asc = true
	}
	sts, err := app.queryStatuses(v, "("+where+")"+extra, append(append([]any{}, args...), eargs...), "", pq.Limit, asc)
	if asc {
		for i, j := 0, len(sts)-1; i < j; i, j = i+1, j-1 {
			sts[i], sts[j] = sts[j], sts[i]
		}
	}
	return sts, err
}

// setLink writes Mastodon's Link pagination header.
func (app *App) setLink(w http.ResponseWriter, r *http.Request, firstID, lastID string, full bool) {
	if firstID == "" {
		return
	}
	base := app.cfg.BaseURL() + r.URL.Path
	q := r.URL.Query()
	q.Del("max_id")
	q.Del("min_id")
	q.Del("since_id")
	var links []string
	if full {
		n := url.Values{}
		for k, v := range q {
			n[k] = v
		}
		n.Set("max_id", lastID)
		links = append(links, "<"+base+"?"+n.Encode()+`>; rel="next"`)
	}
	p := url.Values{}
	for k, v := range q {
		p[k] = v
	}
	p.Set("min_id", firstID)
	links = append(links, "<"+base+"?"+p.Encode()+`>; rel="prev"`)
	w.Header().Set("Link", strings.Join(links, ", "))
}

func (app *App) writeStatuses(w http.ResponseWriter, r *http.Request, v *Account, sts []*Status, pq pageQ) {
	if len(sts) > 0 {
		app.setLink(w, r, sts[0].ID, sts[len(sts)-1].ID, len(sts) == pq.Limit)
	}
	apiJSON(w, 200, app.apiStatuses(v, sts))
}

func (app *App) writeAccounts(w http.ResponseWriter, accs []*Account) {
	out := []any{}
	for _, a := range accs {
		out = append(out, app.apiAccount(a))
	}
	apiJSON(w, 200, out)
}

func emptyList(w http.ResponseWriter, r *http.Request, c *apiCtx) { apiJSON(w, 200, []any{}) }

// ---- instance ----

func (app *App) apiInstance(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	apiJSON(w, 200, app.apiInstanceV1())
}

func (app *App) apiInstance2(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	apiJSON(w, 200, app.apiInstanceV2())
}

func (app *App) apiPeers(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	rows, err := app.db.Query(`SELECT DISTINCT domain FROM accounts WHERE domain != '' AND suspended=0 ORDER BY domain LIMIT 1000`)
	out := []string{}
	if err == nil {
		for rows.Next() {
			var d string
			rows.Scan(&d)
			out = append(out, d)
		}
		rows.Close()
	}
	apiJSON(w, 200, out)
}

func (app *App) apiPreferences(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	apiJSON(w, 200, obj{"posting:default:visibility": "public", "posting:default:sensitive": false, "posting:default:language": nil,
		"reading:expand:media": "default", "reading:expand:spoilers": false})
}

func (app *App) apiGetMarkers(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := obj{}
	rows, err := app.db.Query(`SELECT timeline, last_read_id, version, updated_at FROM markers WHERE account_id=?`, c.Me.ID)
	if err == nil {
		for rows.Next() {
			var tl, id string
			var ver int
			var upd int64
			rows.Scan(&tl, &id, &ver, &upd)
			out[tl] = obj{"last_read_id": id, "version": ver, "updated_at": apiTime(time.Unix(upd, 0))}
		}
		rows.Close()
	}
	apiJSON(w, 200, out)
}

func (app *App) apiSetMarkers(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	p := readParams(r)
	for _, tl := range []string{"home", "notifications"} {
		if id := p.Get(tl + ".last_read_id"); id != "" {
			app.db.Exec(`INSERT INTO markers (account_id, timeline, last_read_id, version, updated_at) VALUES (?,?,?,0,?)
				ON CONFLICT(account_id, timeline) DO UPDATE SET last_read_id=excluded.last_read_id, version=version+1, updated_at=excluded.updated_at`,
				c.Me.ID, tl, id, time.Now().Unix())
			if tl == "notifications" {
				app.db.Exec(`UPDATE notifications SET read=1 WHERE account_id=? AND id <= ?`, c.Me.ID, id)
			}
		}
	}
	app.apiGetMarkers(w, r, c)
}

// ---- accounts ----

func (app *App) apiVerifyCredentials(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	apiJSON(w, 200, app.apiCredentialAccount(c.Me))
}

func (app *App) apiUpdateCredentials(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	p := readParams(r)
	me := c.Me
	if p.Has("display_name") {
		me.DisplayName = truncateRunes(strings.TrimSpace(p.Get("display_name")), 100)
	}
	if p.Has("note") {
		me.NoteSource = truncateRunes(strings.TrimSpace(p.Get("note")), 2000)
		me.Note = app.formatBio(r.Context(), me.NoteSource)
	}
	if p.Has("locked") {
		me.Locked = p.Bool("locked")
	}
	if p.Has("bot") {
		if p.Bool("bot") {
			me.ActorType = "Service"
		} else {
			me.ActorType = "Person"
		}
	}
	// fields_attributes[0][name]=..., or JSON array/object
	var fields []Field
	seenFields := false
	for i := 0; i < 4; i++ {
		k := "fields_attributes." + strconv.Itoa(i)
		if p.Has(k+".name") || p.Has(k+".value") {
			seenFields = true
			if n, v := strings.TrimSpace(p.Get(k+".name")), strings.TrimSpace(p.Get(k+".value")); n != "" || v != "" {
				fields = append(fields, Field{Name: truncateRunes(n, 100), Value: fieldHTML(truncateRunes(v, 500))})
			}
		}
	}
	if raw, ok := p.jsonPath("fields_attributes"); ok {
		if list, ok := raw.([]any); ok {
			seenFields = true
			fields = nil
			for _, e := range list {
				if m, ok := e.(map[string]any); ok && len(fields) < 4 {
					n, _ := m["name"].(string)
					v, _ := m["value"].(string)
					if n != "" || v != "" {
						fields = append(fields, Field{Name: truncateRunes(n, 100), Value: fieldHTML(truncateRunes(v, 500))})
					}
				}
			}
		}
	}
	if seenFields {
		me.Fields = fields
	}
	for _, f := range []string{"avatar", "header"} {
		if r.MultipartForm == nil || len(r.MultipartForm.File[f]) == 0 {
			continue
		}
		fh, err := r.MultipartForm.File[f][0].Open()
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(fh, maxUploadSize+1))
		fh.Close()
		old := me.Avatar
		if f == "header" {
			old = me.Header
		}
		u, err := app.replaceImage(me, old, data)
		if err != nil {
			apiError(w, 422, f+": "+err.Error())
			return
		}
		if f == "avatar" {
			me.Avatar = u
		} else {
			me.Header = u
		}
	}
	if err := app.UpdateProfile(me); err != nil {
		apiError(w, 500, err.Error())
		return
	}
	apiJSON(w, 200, app.apiCredentialAccount(me))
}

func (app *App) apiAccountByID(w http.ResponseWriter, r *http.Request) (*Account, bool) {
	a, err := app.AccountByID(r.PathValue("id"))
	if err != nil || a.Suspended || (a.ActorType == "Application" && a.IsLocal()) {
		apiError(w, 404, "Record not found")
		return nil, false
	}
	return a, true
}

func (app *App) apiGetAccount(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if a, ok := app.apiAccountByID(w, r); ok {
		apiJSON(w, 200, app.apiAccount(a))
	}
}

func (app *App) apiAccountStatuses(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := app.apiAccountByID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	pq := pageParams(r, 20, 40)
	if q.Get("pinned") == "true" {
		apiJSON(w, 200, app.apiStatuses(c.Me, app.pinnedStatuses(c.Me, a)))
		return
	}
	where := `s.account_id=?`
	args := []any{a.ID}
	if q.Get("exclude_replies") == "true" {
		where += ` AND (s.in_reply_to_uri='' OR s.in_reply_to_account_id=s.account_id)`
	}
	if q.Get("exclude_reblogs") == "true" {
		where += ` AND s.boost_of_id=''`
	}
	if q.Get("only_media") == "true" {
		where += ` AND s.boost_of_id='' AND EXISTS(SELECT 1 FROM attachments at WHERE at.status_id=s.id)`
	}
	if t := q.Get("tagged"); t != "" {
		where += ` AND s.id IN (SELECT status_id FROM status_tags WHERE tag=?)`
		args = append(args, strings.ToLower(t))
	}
	if !a.IsLocal() && c.Me != nil && pq.MaxID == "" && app.countInt(`SELECT COUNT(*) FROM statuses WHERE account_id=?`, a.ID) < 5 {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		app.backfillOutbox(ctx, a)
		cancel()
	}
	sts, err := app.pageStatuses(c.Me, where, args, pq)
	if err != nil {
		apiError(w, 500, "db error")
		return
	}
	app.writeStatuses(w, r, c.Me, sts, pq)
}

func (app *App) apiFollowList(followers bool) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, c *apiCtx) {
		a, ok := app.apiAccountByID(w, r)
		if !ok {
			return
		}
		list, _ := app.followList(a, followers, false, 0)
		app.writeAccounts(w, list)
	}
}

func (app *App) apiAccountAction(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	target, ok := app.apiAccountByID(w, r)
	if !ok {
		return
	}
	var err error
	switch r.PathValue("action") {
	case "follow":
		err = app.Follow(c.Me, target)
	case "unfollow":
		err = app.Unfollow(c.Me, target)
	case "block":
		err = app.Block(c.Me, target)
	case "unblock":
		err = app.Unblock(c.Me, target)
	case "mute":
		err = app.Mute(c.Me, target)
	case "unmute":
		err = app.Unmute(c.Me, target)
	case "remove_from_followers":
		err = app.RejectFollow(c.Me, target)
		if errors.Is(err, errNotFound) {
			err = nil
		}
	case "note", "pin", "unpin", "endorse", "unendorse":
	default:
		apiError(w, 404, "Record not found")
		return
	}
	if err != nil {
		apiError(w, 422, err.Error())
		return
	}
	apiJSON(w, 200, app.apiRelationship(c.Me, target))
}

func (app *App) apiRelationships(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	q := r.URL.Query()
	ids := append(q["id[]"], q["id"]...)
	out := []any{}
	for _, id := range ids {
		if a, err := app.AccountByID(id); err == nil {
			out = append(out, app.apiRelationship(c.Me, a))
		}
	}
	apiJSON(w, 200, out)
}

func (app *App) apiLookup(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	user, domain, ok := parseHandle(r.URL.Query().Get("acct"))
	if !ok {
		apiError(w, 404, "Record not found")
		return
	}
	acc, err := app.AccountByAcct(user, domain)
	if err != nil && c.Me != nil && domain != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		acc, err = app.resolveAcct(ctx, user, domain)
		cancel()
	}
	if err != nil || acc.Suspended {
		apiError(w, 404, "Record not found")
		return
	}
	apiJSON(w, 200, app.apiAccount(acc))
}

func (app *App) searchAccounts(ctx context.Context, c *apiCtx, q string, resolve bool, limit int) []*Account {
	q = strings.TrimSpace(q)
	if resolve && c.Me != nil {
		if strings.HasPrefix(q, "https://") || strings.HasPrefix(q, "http://") {
			if acc, _, err := app.resolveURL(ctx, q); err == nil && acc != nil {
				return []*Account{acc}
			}
		} else if user, domain, ok := parseHandle(q); ok && domain != "" {
			if acc, err := app.resolveAcct(ctx, user, domain); err == nil {
				return []*Account{acc}
			}
		}
	}
	term := strings.TrimPrefix(q, "@")
	user, domain, _ := strings.Cut(term, "@")
	like := "%" + strings.NewReplacer("%", `\%`, "_", `\_`).Replace(user) + "%"
	where := `a.suspended=0 AND a.actor_type != 'Application' AND (a.username LIKE ? ESCAPE '\' OR a.display_name LIKE ? ESCAPE '\')`
	args := []any{like, like}
	if domain != "" {
		where += ` AND a.domain LIKE ?`
		args = append(args, strings.ToLower(domain)+"%")
	}
	accs, _ := app.accountsQuery(`SELECT `+accountCols+` FROM accounts a WHERE `+where+` ORDER BY a.domain != '', length(a.username) LIMIT ?`, append(args, limit)...)
	return accs
}

func (app *App) apiAccountSearch(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 40 {
		limit = 40
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	app.writeAccounts(w, app.searchAccounts(ctx, c, q.Get("q"), q.Get("resolve") == "true", limit))
}

func (app *App) apiBlocksOrMutes(table string) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, c *apiCtx) {
		accs, _ := app.accountsQuery(`SELECT `+accountCols+` FROM `+table+` x JOIN accounts a ON a.id=x.target_id WHERE x.account_id=? ORDER BY x.created_at DESC LIMIT 80`, c.Me.ID)
		app.writeAccounts(w, accs)
	}
}

func (app *App) apiFollowRequests(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	list, _ := app.followList(c.Me, true, true, 0)
	app.writeAccounts(w, list)
}

func (app *App) apiFollowRequestAction(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	other, ok := app.apiAccountByID(w, r)
	if !ok {
		return
	}
	var err error
	if r.PathValue("action") == "authorize" {
		err = app.AcceptFollow(c.Me, other)
	} else {
		err = app.RejectFollow(c.Me, other)
	}
	if err != nil && !errors.Is(err, errNotFound) {
		apiError(w, 422, err.Error())
		return
	}
	apiJSON(w, 200, app.apiRelationship(c.Me, other))
}

func (app *App) apiBookmarks(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	pq := pageParams(r, 20, 40)
	sts, _ := app.pageStatuses(c.Me, `s.id IN (SELECT status_id FROM bookmarks WHERE account_id=?)`, []any{c.Me.ID}, pq)
	app.writeStatuses(w, r, c.Me, sts, pq)
}

func (app *App) apiFavourites(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	pq := pageParams(r, 20, 40)
	sts, _ := app.pageStatuses(c.Me, `s.id IN (SELECT status_id FROM likes WHERE account_id=?)`, []any{c.Me.ID}, pq)
	app.writeStatuses(w, r, c.Me, sts, pq)
}

// ---- timelines ----

func (app *App) apiHome(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	pq := pageParams(r, 20, 40)
	where, args := homeSQL(c.Me)
	sts, err := app.pageStatuses(c.Me, where, args, pq)
	if err != nil {
		apiError(w, 500, "db error")
		return
	}
	app.writeStatuses(w, r, c.Me, app.applyFilters(c.Me, sts), pq)
}

func (app *App) apiPublic(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	q := r.URL.Query()
	pq := pageParams(r, 20, 40)
	where := `s.visibility='public' AND s.boost_of_id=''`
	switch {
	case q.Get("local") == "true":
		where += ` AND s.local=1`
	case q.Get("remote") == "true":
		where += ` AND s.local=0`
	}
	if c.Me == nil && q.Get("local") != "true" {
		apiError(w, 401, "This method requires an authenticated user")
		return
	}
	if q.Get("only_media") == "true" {
		where += ` AND EXISTS(SELECT 1 FROM attachments at WHERE at.status_id=s.id)`
	}
	mute, margs := muteSQL(c.Me)
	sts, err := app.pageStatuses(c.Me, where+` AND `+mute, margs, pq)
	if err != nil {
		apiError(w, 500, "db error")
		return
	}
	app.writeStatuses(w, r, c.Me, app.applyFilters(c.Me, sts), pq)
}

func (app *App) apiTagTimeline(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	pq := pageParams(r, 20, 40)
	where := `s.visibility='public' AND s.id IN (SELECT status_id FROM status_tags WHERE tag=?)`
	if r.URL.Query().Get("local") == "true" {
		where += ` AND s.local=1`
	}
	mute, margs := muteSQL(c.Me)
	sts, err := app.pageStatuses(c.Me, where+` AND `+mute, append([]any{strings.ToLower(r.PathValue("tag"))}, margs...), pq)
	if err != nil {
		apiError(w, 500, "db error")
		return
	}
	app.writeStatuses(w, r, c.Me, app.applyFilters(c.Me, sts), pq)
}

// ---- statuses ----

func (app *App) apiLoadStatus(w http.ResponseWriter, r *http.Request, c *apiCtx) (*Status, bool) {
	st, err := app.loadStatus(c.Me, r.PathValue("id"))
	if err != nil {
		apiError(w, 404, "Record not found")
		return nil, false
	}
	return st, true
}

func (app *App) apiGetStatus(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if st, ok := app.apiLoadStatus(w, r, c); ok {
		apiJSON(w, 200, app.apiStatus(c.Me, st))
	}
}

var (
	idemMu   sync.Mutex
	idemKeys = map[string]idemEntry{}
)

type idemEntry struct {
	statusID string
	at       time.Time
}

func (app *App) apiPostStatus(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	p := readParams(r)
	idem := r.Header.Get("Idempotency-Key")
	if idem != "" {
		key := c.Me.ID + "|" + idem
		idemMu.Lock()
		e, ok := idemKeys[key]
		for k, v := range idemKeys {
			if time.Since(v.at) > time.Hour {
				delete(idemKeys, k)
			}
		}
		idemMu.Unlock()
		if ok {
			if st, err := app.loadStatus(c.Me, e.statusID); err == nil {
				apiJSON(w, 200, app.apiStatus(c.Me, st))
				return
			}
		}
	}
	in := PostInput{
		Text: p.Get("status"), Spoiler: p.Get("spoiler_text"), Visibility: p.Get("visibility"),
		Sensitive: p.Bool("sensitive"), InReplyTo: p.Get("in_reply_to_id"), MediaIDs: p.List("media_ids"),
		QuoteOf: p.Get("quoted_status_id"),
	}
	if in.QuoteOf == "" {
		in.QuoteOf = p.Get("quote_id")
	}
	if in.Visibility == "" {
		in.Visibility = "public"
	}
	if opts := p.List("poll.options"); len(opts) > 0 {
		poll, err := newPollSeconds(opts, p.Int("poll.expires_in", 86400), p.Bool("poll.multiple"))
		if err != nil {
			apiError(w, 422, err.Error())
			return
		}
		in.Poll = poll
	}
	if in.Visibility == "private" || in.Visibility == "direct" || in.Visibility == "public" || in.Visibility == "unlisted" {
	} else {
		apiError(w, 422, "Validation failed: visibility is invalid")
		return
	}
	if when := p.Get("scheduled_at"); when != "" {
		at, err := time.Parse(time.RFC3339, when)
		if err != nil {
			apiError(w, 422, "Validation failed: scheduled_at is not a valid time")
			return
		}
		in.PollStart = time.Now()
		s, err := app.Schedule(c.Me, in, at)
		if err != nil {
			apiError(w, 422, "Validation failed: "+err.Error())
			return
		}
		list, _ := app.scheduledFor(c.Me, s.ID)
		apiJSON(w, 200, app.apiScheduled(list[0]))
		return
	}
	st, err := app.PostStatus(r.Context(), c.Me, in)
	if err != nil {
		apiError(w, 422, "Validation failed: "+err.Error())
		return
	}
	if idem != "" {
		idemMu.Lock()
		idemKeys[c.Me.ID+"|"+idem] = idemEntry{st.ID, time.Now()}
		idemMu.Unlock()
	}
	apiJSON(w, 200, app.apiStatus(c.Me, st))
}

func newPollSeconds(options []string, seconds int, multiple bool) (*Poll, error) {
	if len(options) > 4 {
		return nil, errors.New("polls have at most 4 options")
	}
	seconds = max(300, min(seconds, 7*24*3600))
	p, err := newPoll(options, "", multiple)
	if err != nil || p == nil {
		return p, err
	}
	p.EndTime = time.Now().Add(time.Duration(seconds) * time.Second).UTC().Truncate(time.Second)
	return p, nil
}

func (app *App) apiEditStatus(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := app.apiLoadStatus(w, r, c)
	if !ok {
		return
	}
	p := readParams(r)
	edited, err := app.EditStatus(r.Context(), c.Me, st, p.Get("status"), p.Get("spoiler_text"), p.Bool("sensitive"))
	if err != nil {
		apiError(w, 422, err.Error())
		return
	}
	apiJSON(w, 200, app.apiStatus(c.Me, edited))
}

func (app *App) apiDeleteStatus(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := app.apiLoadStatus(w, r, c)
	if !ok {
		return
	}
	if st.AccountID != c.Me.ID {
		apiError(w, 403, "This action is not allowed")
		return
	}
	out := app.apiStatus(c.Me, st)
	out["text"] = st.Source
	if err := app.DeleteStatus(c.Me, st); err != nil {
		apiError(w, 422, err.Error())
		return
	}
	apiJSON(w, 200, out)
}

func (app *App) apiStatusSource(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := app.apiLoadStatus(w, r, c)
	if !ok {
		return
	}
	text := st.Source
	if text == "" {
		text = plainText(st.Content)
	}
	apiJSON(w, 200, obj{"id": st.ID, "text": text, "spoiler_text": st.Spoiler})
}

func (app *App) apiStatusHistory(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := app.apiLoadStatus(w, r, c)
	if !ok {
		return
	}
	media := []any{}
	for _, a := range st.Attachments {
		media = append(media, app.apiMedia(a))
	}
	out := []any{}
	for _, v := range app.history(st) {
		out = append(out, obj{"content": v.Content, "spoiler_text": v.Spoiler, "sensitive": v.Sensitive, "created_at": apiTime(v.At),
			"account": app.apiAccount(st.Account), "media_attachments": media, "emojis": app.apiEmojis(st.Emojis), "poll": nil})
	}
	apiJSON(w, 200, out)
}

func (app *App) apiContext(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := app.apiLoadStatus(w, r, c)
	if !ok {
		return
	}
	if c.Me != nil && st.InReplyToURI != "" && st.InReplyToID == "" {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		if parent, err := app.resolveStatus(ctx, st.InReplyToURI); err == nil {
			st.InReplyToID = parent.ID
		}
		cancel()
	}
	anc, desc := app.thread(c.Me, st)
	apiJSON(w, 200, obj{"ancestors": app.apiStatuses(c.Me, anc), "descendants": app.apiStatuses(c.Me, desc)})
}

func (app *App) apiStatusAction(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := app.apiLoadStatus(w, r, c)
	if !ok {
		return
	}
	target := st
	if st.IsBoost() {
		target = st.BoostOf
	}
	var err error
	switch r.PathValue("action") {
	case "favourite":
		err = app.Like(c.Me, target)
	case "unfavourite":
		err = app.Unlike(c.Me, target)
	case "reblog":
		err = app.Boost(c.Me, target)
	case "unreblog":
		err = app.Unboost(c.Me, target)
	case "bookmark":
		err = app.Bookmark(c.Me, target)
	case "unbookmark":
		err = app.Unbookmark(c.Me, target)
	case "pin":
		err = app.Pin(c.Me, target)
	case "unpin":
		err = app.Unpin(c.Me, target)
	case "mute", "unmute":
	default:
		apiError(w, 404, "Record not found")
		return
	}
	if err != nil {
		apiError(w, 422, err.Error())
		return
	}
	fresh, err := app.loadStatus(c.Me, target.ID)
	if err != nil {
		apiError(w, 404, "Record not found")
		return
	}
	if r.PathValue("action") == "reblog" {
		// Mastodon returns the boost wrapping the original
		if b, err := app.statusWhere(`s.account_id=? AND s.boost_of_id=?`, c.Me.ID, target.ID); err == nil {
			if bs, err := app.loadStatus(c.Me, b.ID); err == nil {
				apiJSON(w, 200, app.apiStatus(c.Me, bs))
				return
			}
		}
	}
	apiJSON(w, 200, app.apiStatus(c.Me, fresh))
}

func (app *App) apiReaction(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := app.apiLoadStatus(w, r, c)
	if !ok {
		return
	}
	emoji := r.PathValue("emoji")
	var err error
	if r.Method == http.MethodDelete {
		err = app.Unreact(c.Me, st, emoji)
	} else {
		err = app.React(c.Me, st, emoji)
	}
	if err != nil {
		apiError(w, 422, err.Error())
		return
	}
	fresh, _ := app.loadStatus(c.Me, st.ID)
	apiJSON(w, 200, app.apiStatus(c.Me, fresh))
}

func (app *App) apiStatusAccounts(likes bool) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, c *apiCtx) {
		st, ok := app.apiLoadStatus(w, r, c)
		if !ok {
			return
		}
		q := `SELECT ` + accountCols + ` FROM likes l JOIN accounts a ON a.id=l.account_id WHERE l.status_id=? ORDER BY l.created_at DESC LIMIT 80`
		if !likes {
			q = `SELECT ` + accountCols + ` FROM statuses b JOIN accounts a ON a.id=b.account_id WHERE b.boost_of_id=? ORDER BY b.id DESC LIMIT 80`
		}
		accs, _ := app.accountsQuery(q, st.ID)
		app.writeAccounts(w, accs)
	}
}

// ---- polls ----

func (app *App) apiGetPoll(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := app.apiLoadStatus(w, r, c)
	if !ok || st.Poll == nil {
		if ok {
			apiError(w, 404, "Record not found")
		}
		return
	}
	apiJSON(w, 200, app.apiPoll(st))
}

func (app *App) apiVote(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := app.apiLoadStatus(w, r, c)
	if !ok {
		return
	}
	var choices []int
	for _, s := range readParams(r).List("choices") {
		if n, err := strconv.Atoi(s); err == nil {
			choices = append(choices, n)
		}
	}
	if err := app.Vote(c.Me, st, choices); err != nil {
		apiError(w, 422, err.Error())
		return
	}
	fresh, _ := app.loadStatus(c.Me, st.ID)
	apiJSON(w, 200, app.apiPoll(fresh))
}

// ---- media ----

func (app *App) apiUploadMedia(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if err := r.ParseMultipartForm(8 << 20); err != nil || r.MultipartForm == nil || len(r.MultipartForm.File["file"]) == 0 {
		apiError(w, 422, "Validation failed: file is missing")
		return
	}
	fh, err := r.MultipartForm.File["file"][0].Open()
	if err != nil {
		apiError(w, 422, "could not read file")
		return
	}
	data, _ := io.ReadAll(io.LimitReader(fh, maxUploadSize+1))
	fh.Close()
	at, err := app.saveUpload(c.Me, data, r.FormValue("description"))
	if err != nil {
		apiError(w, 422, "Validation failed: "+err.Error())
		return
	}
	apiJSON(w, 200, app.apiMedia(at))
}

func (app *App) apiMediaByID(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	at := &Attachment{}
	err := app.db.QueryRow(`SELECT id, account_id, url, media_type, description, file, width, height FROM attachments WHERE id=? AND account_id=?`,
		r.PathValue("id"), c.Me.ID).Scan(&at.ID, &at.AccountID, &at.URL, &at.MediaType, &at.Description, &at.File, &at.Width, &at.Height)
	if err != nil {
		apiError(w, 404, "Record not found")
		return
	}
	if r.Method == http.MethodPut {
		if d := readParams(r).Get("description"); true {
			at.Description = truncateRunes(strings.TrimSpace(d), 1500)
			app.db.Exec(`UPDATE attachments SET description=? WHERE id=?`, at.Description, at.ID)
		}
	}
	apiJSON(w, 200, app.apiMedia(at))
}

// ---- notifications ----

// notificationFilter turns the Mastodon query parameters shared by the v1
// and v2 notification endpoints into SQL. none means nothing can match.
func (app *App) notificationFilter(r *http.Request, c *apiCtx, pq pageQ) (where string, args []any, order string, none bool) {
	q := r.URL.Query()
	where = `n.account_id=? AND a.suspended=0 AND NOT EXISTS(SELECT 1 FROM mutes mu WHERE mu.account_id=n.account_id AND mu.target_id=n.from_account_id)`
	args = []any{c.Me.ID}
	reverse := map[string]string{}
	for k, v := range notificationTypes {
		reverse[v] = k
	}
	if types := append(q["types[]"], q["types"]...); len(types) > 0 {
		var ours []string
		for _, t := range types {
			if o, ok := reverse[t]; ok {
				ours = append(ours, o)
			}
		}
		if len(ours) == 0 {
			return "", nil, "", true
		}
		ph, a := inClause(`%s`, ours)
		where += ` AND n.type IN (` + ph + `)`
		args = append(args, a...)
	}
	if ex := append(q["exclude_types[]"], q["exclude_types"]...); len(ex) > 0 {
		var ours []string
		for _, t := range ex {
			if o, ok := reverse[t]; ok {
				ours = append(ours, o)
			}
		}
		if len(ours) > 0 {
			ph, a := inClause(`%s`, ours)
			where += ` AND n.type NOT IN (` + ph + `)`
			args = append(args, a...)
		}
	}
	if id := q.Get("account_id"); id != "" {
		where += ` AND n.from_account_id=?`
		args = append(args, id)
	}
	order = "DESC"
	if pq.MaxID != "" {
		where += ` AND n.id < ?`
		args = append(args, pq.MaxID)
	}
	if pq.SinceID != "" {
		where += ` AND n.id > ?`
		args = append(args, pq.SinceID)
	}
	if pq.MinID != "" {
		where += ` AND n.id > ?`
		args = append(args, pq.MinID)
		order = "ASC"
	}
	return where, args, order, false
}

func (app *App) apiNotifications(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	pq := pageParams(r, 15, 80)
	where, args, order, none := app.notificationFilter(r, c, pq)
	if none {
		apiJSON(w, 200, []any{})
		return
	}
	ns, err := app.notificationsWhere(c.Me, where, args, order, pq.Limit)
	if err != nil {
		apiError(w, 500, "db error")
		return
	}
	out := []any{}
	for _, n := range ns {
		out = append(out, app.apiNotification(c.Me, n))
	}
	if len(ns) > 0 {
		app.setLink(w, r, ns[0].ID, ns[len(ns)-1].ID, len(ns) == pq.Limit)
	}
	apiJSON(w, 200, out)
}

func (app *App) apiNotification1(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	ns, err := app.notificationsWhere(c.Me, `n.account_id=? AND n.id=?`, []any{c.Me.ID, r.PathValue("id")}, "DESC", 1)
	if err != nil || len(ns) == 0 {
		apiError(w, 404, "Record not found")
		return
	}
	apiJSON(w, 200, app.apiNotification(c.Me, ns[0]))
}

func (app *App) apiClearNotifications(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	app.db.Exec(`DELETE FROM notifications WHERE account_id=?`, c.Me.ID)
	apiJSON(w, 200, obj{})
}

func (app *App) apiDismissNotification(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	app.db.Exec(`DELETE FROM notifications WHERE account_id=? AND id=?`, c.Me.ID, r.PathValue("id"))
	apiJSON(w, 200, obj{})
}

func (app *App) apiUnreadCount(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	apiJSON(w, 200, obj{"count": app.unreadNotifications(c.Me.ID)})
}

// ---- search ----

func (app *App) apiSearch(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	q := r.URL.Query()
	term := strings.TrimSpace(q.Get("q"))
	typ := q.Get("type")
	resolve := q.Get("resolve") == "true" && c.Me != nil
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 40 {
		limit = 20
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	res := obj{"accounts": []any{}, "statuses": []any{}, "hashtags": []any{}}
	if term == "" {
		apiJSON(w, 200, res)
		return
	}
	if typ == "" || typ == "statuses" {
		var sts []*Status
		if resolve && (strings.HasPrefix(term, "https://") || strings.HasPrefix(term, "http://")) {
			if _, st, err := app.resolveURL(ctx, term); err == nil && st != nil {
				if full, err := app.loadStatus(c.Me, st.ID); err == nil {
					sts = append(sts, full)
				}
			}
		} else if c.Me != nil && !strings.HasPrefix(term, "@") && !strings.HasPrefix(term, "#") {
			like := "%" + strings.NewReplacer("%", `\%`, "_", `\_`).Replace(term) + "%"
			sts, _ = app.queryStatuses(c.Me, `s.boost_of_id='' AND s.visibility IN ('public','unlisted') AND s.content LIKE ? ESCAPE '\'`, []any{like}, "", limit, false)
		}
		res["statuses"] = app.apiStatuses(c.Me, sts)
	}
	if typ == "" || typ == "accounts" {
		out := []any{}
		for _, a := range app.searchAccounts(ctx, c, term, resolve, limit) {
			out = append(out, app.apiAccount(a))
		}
		res["accounts"] = out
	}
	if typ == "" || typ == "hashtags" {
		tag := strings.ToLower(strings.TrimPrefix(term, "#"))
		out := []any{}
		if tag != "" && !strings.ContainsAny(tag, " @/") {
			rows, err := app.db.Query(`SELECT tag, COUNT(*) FROM status_tags WHERE tag LIKE ? GROUP BY tag ORDER BY COUNT(*) DESC LIMIT ?`, tag+"%", limit)
			if err == nil {
				for rows.Next() {
					var t string
					var n int
					rows.Scan(&t, &n)
					out = append(out, obj{"name": t, "url": app.cfg.BaseURL() + "/tags/" + t, "history": []any{}, "following": false})
				}
				rows.Close()
			}
		}
		res["hashtags"] = out
	}
	apiJSON(w, 200, res)
}

// ---- filters (read-only mapping of our keyword filters) ----

func (app *App) apiFiltersV2(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	for _, f := range app.filtersFor(c.Me.ID) {
		action := "warn"
		if f.Action == "hide" {
			action = "hide"
		}
		out = append(out, obj{"id": f.ID, "title": f.Phrase, "context": []string{"home", "public", "notifications", "thread", "account"},
			"expires_at": nil, "filter_action": action, "keywords": []any{obj{"id": f.ID, "keyword": f.Phrase, "whole_word": f.WholeWord}},
			"statuses": []any{}})
	}
	apiJSON(w, 200, out)
}

func (app *App) apiFiltersV1(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	for _, f := range app.filtersFor(c.Me.ID) {
		out = append(out, obj{"id": f.ID, "phrase": f.Phrase, "context": []string{"home", "public", "notifications", "thread", "account"},
			"whole_word": f.WholeWord, "expires_at": nil, "irreversible": f.Action == "hide"})
	}
	apiJSON(w, 200, out)
}

// ---- conversations (direct messages) ----

func (app *App) apiConversations(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	pq := pageParams(r, 20, 40)
	sts, _ := app.pageStatuses(c.Me, `s.visibility='direct' AND (s.account_id=? OR EXISTS(SELECT 1 FROM mentions m WHERE m.status_id=s.id AND m.account_id=?))`,
		[]any{c.Me.ID, c.Me.ID}, pq)
	out := []any{}
	for _, s := range sts {
		var accs []any
		seen := map[string]bool{c.Me.ID: true}
		for _, a := range append([]*Account{s.Account}, s.Mentions...) {
			if !seen[a.ID] {
				seen[a.ID] = true
				accs = append(accs, app.apiAccount(a))
			}
		}
		if accs == nil {
			accs = []any{}
		}
		out = append(out, obj{"id": s.ID, "unread": false, "accounts": accs, "last_status": app.apiStatus(c.Me, s)})
	}
	if len(sts) > 0 {
		app.setLink(w, r, sts[0].ID, sts[len(sts)-1].ID, len(sts) == pq.Limit)
	}
	apiJSON(w, 200, out)
}
