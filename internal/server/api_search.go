package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

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
