package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

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
	if p.Has("discoverable") {
		me.Discoverable = p.Bool("discoverable")
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
	if p.Has("source.quote_policy") {
		if err := app.SetDefaultQuotePolicy(me, p.Get("source.quote_policy")); err != nil {
			apiError(w, 422, "Validation failed: "+err.Error())
			return
		}
	}
	if p.Has("attribution_domains") || p.Has("attribution_domains[]") {
		domains, err := attributionDomains(p.List("attribution_domains"))
		if err != nil {
			apiError(w, 422, "Validation failed: "+err.Error())
			return
		}
		me.AttributionDomains = domains
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

// apiDeleteProfileImage removes the avatar or header
// (DELETE /api/v1/profile/avatar, /api/v1/profile/header).
func (app *App) apiDeleteProfileImage(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	me := c.Me
	if r.PathValue("image") == "avatar" {
		app.replaceRemove(me.Avatar)
		me.Avatar = ""
	} else {
		app.replaceRemove(me.Header)
		me.Header = ""
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

// apiAccounts returns several accounts at once (GET /api/v1/accounts?id[]=,
// Mastodon 4.3); unknown ids are left out.
func (app *App) apiAccounts(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	q := r.URL.Query()
	out := []any{}
	for i, id := range append(q["id[]"], q["id"]...) {
		if i >= 40 {
			break
		}
		if a, err := app.AccountByID(id); err == nil && !a.Suspended && !(a.ActorType == "Application" && a.IsLocal()) {
			out = append(out, app.apiAccount(a))
		}
	}
	apiJSON(w, 200, out)
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
	app.writeStatuses(w, r, c.Me, app.applyFilters(c.Me, sts, "account"), pq)
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

// apiEndorsements lists the accounts someone features on their profile
// (GET /api/v1/accounts/:id/endorsements, Mastodon 4.4).
func (app *App) apiEndorsements(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := app.apiAccountByID(w, r)
	if !ok {
		return
	}
	list, _ := app.endorsements(a.ID)
	app.writeAccounts(w, list)
}

func (app *App) apiMyEndorsements(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	list, _ := app.endorsements(c.Me.ID)
	app.writeAccounts(w, list)
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
		if err == nil {
			p := readParams(r)
			var reblogs, notify *bool
			if p.Has("reblogs") {
				b := p.Get("reblogs") != "false"
				reblogs = &b
			}
			if p.Has("notify") {
				b := p.Get("notify") == "true"
				notify = &b
			}
			app.SetFollowOptions(c.Me, target, reblogs, notify)
		}
	case "unfollow":
		err = app.Unfollow(c.Me, target)
	case "block":
		err = app.Block(c.Me, target)
	case "unblock":
		err = app.Unblock(c.Me, target)
	case "mute":
		p := readParams(r)
		err = app.MuteFor(c.Me, target, p.Get("notifications") != "false", muteDuration(p.Get("duration")))
	case "unmute":
		err = app.Unmute(c.Me, target)
	case "remove_from_followers":
		err = app.RejectFollow(c.Me, target)
		if errors.Is(err, errNotFound) {
			err = nil
		}
	case "note":
		err = app.SetAccountNote(c.Me, target, readParams(r).Get("comment"))
	case "pin", "endorse":
		err = app.Endorse(c.Me, target)
	case "unpin", "unendorse":
		err = app.Unendorse(c.Me, target)
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
		if table != "mutes" {
			app.writeAccounts(w, accs)
			return
		}
		expiry := app.muteExpiry(c.Me)
		out := []any{}
		for _, a := range accs {
			m := app.apiAccount(a)
			m["mute_expires_at"] = apiTime(expiry[a.ID])
			out = append(out, m)
		}
		apiJSON(w, 200, out)
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

// apiReport files a report the way the web form does (POST /api/v1/reports).
func (app *App) apiReport(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	p := readParams(r)
	target, err := app.AccountByID(p.Get("account_id"))
	if err != nil {
		apiError(w, 404, "Record not found")
		return
	}
	statusIDs := p.List("status_ids")
	forward := p.Bool("forward")
	id, err := app.FileReport(c.Me, target, statusIDs, p.Get("comment"), forward)
	if err != nil {
		apiError(w, 422, err.Error())
		return
	}
	var kept string
	app.db.QueryRow(`SELECT status_ids FROM reports WHERE id=?`, id).Scan(&kept)
	var ids []string
	jsonUnmarshal([]byte(kept), &ids)
	if ids == nil {
		ids = []string{}
	}
	category := p.Get("category")
	if category == "" {
		category = "other"
	}
	apiJSON(w, 200, obj{
		"id": id, "action_taken": false, "action_taken_at": nil, "category": category,
		"comment": truncateRunes(strings.TrimSpace(p.Get("comment")), 1000), "forwarded": forward && !target.IsLocal(),
		"created_at": apiTime(time.Now()), "status_ids": ids, "rule_ids": []string{}, "target_account": app.apiAccount(target),
	})
}
