package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (app *App) handleSearch(w http.ResponseWriter, r *http.Request, p *Page) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	p.Title, p.Section, p.Query, p.Empty = "Search", "search", q, "No matching posts."
	if q == "" {
		app.render(w, "search", p)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	switch {
	case strings.HasPrefix(q, "#") && !strings.ContainsAny(q, " "):
		http.Redirect(w, r, "/tags/"+url.PathEscape(strings.ToLower(q[1:])), http.StatusFound)
		return
	case strings.HasPrefix(q, "https://") || strings.HasPrefix(q, "http://"):
		acc, st, err := app.resolveURL(ctx, q)
		if err == nil && acc != nil {
			http.Redirect(w, r, acc.Path(), http.StatusFound)
			return
		}
		if err == nil && st != nil {
			http.Redirect(w, r, st.Path(), http.StatusFound)
			return
		}
		p.Error = "Could not find anything at that address."
		if err != nil {
			p.Error += " (" + err.Error() + ")"
		}
	case strings.Count(q, "@") >= 1 && !strings.Contains(q, " ") && strings.Contains(strings.TrimPrefix(q, "@"), "@"):
		user, domain, ok := parseHandle(q)
		if ok {
			acc, err := app.resolveAcct(ctx, user, domain)
			if err == nil {
				http.Redirect(w, r, acc.Path(), http.StatusFound)
				return
			}
			p.Error = "Could not find " + q + " (" + err.Error() + ")."
		}
	}
	sq := parseStatusQuery(q)
	p.Advanced = sq.HasOperators()
	if text := strings.TrimPrefix(q, "@"); !p.Advanced || sq.Text() != "" {
		if p.Advanced {
			text = sq.Text()
		}
		like := likePattern(text)
		accs, _ := app.accountsQuery(`SELECT `+accountCols+` FROM accounts a WHERE a.suspended=0 AND a.actor_type != 'Application'
			AND (a.username LIKE ? ESCAPE '\' OR a.display_name LIKE ? ESCAPE '\') ORDER BY a.domain != '', a.fetched_at DESC LIMIT 20`, like, like)
		p.Accounts = accs
	}
	if strings.HasPrefix(q, "@") && !p.Advanced {
		q = q[1:]
	}
	p.Statuses = app.searchStatuses(p.Me, q, "", r.URL.Query().Get("max_id"), 20)
	if n := len(p.Statuses); n == 20 {
		p.NextURL = "/search?" + url.Values{"q": {p.Query}, "max_id": {p.Statuses[n-1].ID}}.Encode()
	}
	app.render(w, "search", p)
}

func (app *App) handleAuthorizeInteraction(w http.ResponseWriter, r *http.Request, p *Page) {
	uri := strings.TrimSpace(r.URL.Query().Get("uri"))
	if uri == "" {
		http.Redirect(w, r, "/search", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/search?q="+url.QueryEscape(uri), http.StatusFound)
}
