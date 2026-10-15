package server

import (
	"net/http"
	"strings"
)

func (app *App) apiHome(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	pq := pageParams(r, 20, 40)
	where, args := homeSQL(c.Me)
	sts, err := app.pageStatuses(c.Me, where, args, pq)
	if err != nil {
		apiError(w, 500, "db error")
		return
	}
	app.writeStatuses(w, r, c.Me, app.applyFilters(c.Me, sts, "home"), pq)
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
	app.writeStatuses(w, r, c.Me, app.applyFilters(c.Me, sts, "public"), pq)
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
	app.writeStatuses(w, r, c.Me, app.applyFilters(c.Me, sts, "public"), pq)
}

// apiLinkTimeline lists public posts whose link preview is the given URL
// (GET /api/v1/timelines/link?url=, Mastodon 4.3).
func (app *App) apiLinkTimeline(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	link := strings.TrimSpace(r.URL.Query().Get("url"))
	if link == "" {
		apiError(w, 422, "url is required")
		return
	}
	pq := pageParams(r, 20, 40)
	mute, margs := muteSQL(c.Me)
	sts, err := app.pageStatuses(c.Me, `s.visibility='public' AND s.card_url=? AND `+mute, append([]any{link}, margs...), pq)
	if err != nil {
		apiError(w, 500, "db error")
		return
	}
	app.writeStatuses(w, r, c.Me, app.applyFilters(c.Me, sts, "public"), pq)
}
