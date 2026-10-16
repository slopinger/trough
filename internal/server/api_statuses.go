package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"
)

func (app *App) apiLoadStatus(w http.ResponseWriter, r *http.Request, c *apiCtx) (*Status, bool) {
	st, err := app.loadStatus(c.Me, r.PathValue("id"))
	if err != nil {
		apiError(w, 404, "Record not found")
		return nil, false
	}
	return st, true
}

// apiStatusesByID returns several posts at once (GET /api/v1/statuses?id[]=,
// Mastodon 4.3), leaving out those the viewer may not see.
func (app *App) apiStatusesByID(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	q := r.URL.Query()
	out := []any{}
	for i, id := range append(q["id[]"], q["id"]...) {
		if i >= 20 {
			break
		}
		if st, err := app.loadStatus(c.Me, id); err == nil {
			out = append(out, app.apiStatus(c.Me, st))
		}
	}
	apiJSON(w, 200, out)
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
		QuoteOf: p.Get("quoted_status_id"), Language: p.Get("language"), QuotePolicy: p.Get("quote_approval_policy"),
	}
	if in.QuotePolicy != "" && !quotePolicies[in.QuotePolicy] {
		apiError(w, 422, "Validation failed: quote_approval_policy is invalid")
		return
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
	if qp := p.Get("quote_approval_policy"); qp != "" && st.AccountID == c.Me.ID {
		if !quotePolicies[qp] {
			apiError(w, 422, "Validation failed: quote_approval_policy is invalid")
			return
		}
		app.db.Exec(`UPDATE statuses SET quote_policy=? WHERE id=?`, qp, st.ID)
		st.QuotePolicy = qp
	}
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
	del := app.DeleteStatusKeepMedia // for "delete and redraft"
	if readParams(r).Bool("delete_media") {
		del = app.DeleteStatus
	}
	if err := del(c.Me, st); err != nil {
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
	if c.Me != nil {
		// Mastodon 4.4: tell clients a fetch of more replies is running, so they can poll it
		if id, started := app.fetchRepliesLater(st); id != "" {
			if run, ok := repliesState(id); started || (ok && !run.Finished) {
				w.Header().Set("Mastodon-Async-Refresh", `id="`+id+`", retry=3`)
			}
		}
	}
	anc, desc := app.thread(c.Me, st)
	anc, desc = app.applyFilters(c.Me, anc, "thread"), app.applyFilters(c.Me, desc, "thread")
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
	case "mute":
		err = app.MuteConversation(c.Me, target)
	case "unmute":
		err = app.UnmuteConversation(c.Me, target)
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

// apiStatusQuotes lists the posts quoting a status that the viewer may see
// (GET /api/v1/statuses/:id/quotes, Mastodon 4.5).
func (app *App) apiStatusQuotes(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := app.apiLoadStatus(w, r, c)
	if !ok {
		return
	}
	pq := pageParams(r, 20, 40)
	sts, _ := app.pageStatuses(c.Me, `s.quote_uri=? AND s.boost_of_id='' AND s.quote_state=''`, []any{st.URI}, pq)
	app.writeStatuses(w, r, c.Me, sts, pq)
}

// apiRevokeQuote withdraws the approval for a post quoting one of mine
// (POST /api/v1/statuses/:id/quotes/:quoting_status_id/revoke, Mastodon 4.5).
func (app *App) apiRevokeQuote(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := app.apiLoadStatus(w, r, c)
	if !ok {
		return
	}
	quoting, err := app.loadStatus(c.Me, r.PathValue("quoting"))
	if err != nil || app.RevokeQuote(c.Me, st, quoting) != nil {
		apiError(w, 404, "Record not found")
		return
	}
	quoting, _ = app.loadStatus(c.Me, quoting.ID)
	apiJSON(w, 200, app.apiStatus(c.Me, quoting))
}

// apiInteractionPolicy changes who may quote one of my posts
// (PUT /api/v1/statuses/:id/interaction_policy, Mastodon 4.5).
func (app *App) apiInteractionPolicy(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := app.apiLoadStatus(w, r, c)
	if !ok {
		return
	}
	qp := readParams(r).Get("quote_approval_policy")
	if st.AccountID != c.Me.ID {
		apiError(w, 404, "Record not found")
		return
	}
	if !quotePolicies[qp] {
		apiError(w, 422, "Validation failed: quote_approval_policy is invalid")
		return
	}
	if err := app.SetQuotePolicy(c.Me, st, qp); err != nil {
		apiError(w, 422, err.Error())
		return
	}
	st, _ = app.loadStatus(c.Me, st.ID)
	apiJSON(w, 200, app.apiStatus(c.Me, st))
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
