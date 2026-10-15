package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

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
