package server

import (
	"net/http"
	"time"
)

// Trend review, like Mastodon's admin trends: admins can hide a hashtag,
// link or post from Explore and the trends API, or approve it. With review
// on (setting trends_review), only approved ones trend at all; otherwise
// everything but what's hidden does. Reviewing happens on /admin/trends,
// which lists the candidates whatever their state.

var trendKinds = map[string]bool{"tag": true, "link": true, "status": true}

// trendFilter is the SQL keeping what may trend publicly; col is the
// column holding the key for kind (a constant from the caller).
func (app *App) trendFilter(kind, col string) string {
	if app.getSetting("trends_review") == "1" {
		return ` AND ` + col + ` IN (SELECT key FROM trend_reviews WHERE kind='` + kind + `' AND state='approved')`
	}
	return ` AND ` + col + ` NOT IN (SELECT key FROM trend_reviews WHERE kind='` + kind + `' AND state='rejected')`
}

func (app *App) reviewTrend(admin *Account, kind, key, action string) {
	switch {
	case !trendKinds[kind] || key == "":
	case action == "reset":
		app.db.Exec(`DELETE FROM trend_reviews WHERE kind=? AND key=?`, kind, key)
	case action == "approve" || action == "reject":
		state := map[string]string{"approve": "approved", "reject": "rejected"}[action]
		app.db.Exec(`INSERT INTO trend_reviews (kind, key, state, reviewed_at, reviewed_by) VALUES (?,?,?,?,?)
			ON CONFLICT(kind, key) DO UPDATE SET state=excluded.state, reviewed_at=excluded.reviewed_at, reviewed_by=excluded.reviewed_by`,
			kind, key, state, time.Now().Unix(), admin.ID)
	}
}

// trendStates maps "kind:key" to approved or rejected.
func (app *App) trendStates() map[string]string {
	out := map[string]string{}
	rows, err := app.db.Query(`SELECT kind, key, state FROM trend_reviews`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var kind, key, state string
		if rows.Scan(&kind, &key, &state) == nil {
			out[kind+":"+key] = state
		}
	}
	return out
}

// pendingTrends counts the candidates nobody has reviewed yet (with review on).
func (app *App) pendingTrends() int {
	if app.getSetting("trends_review") != "1" {
		return 0
	}
	unreviewed := func(kind, col string) string {
		return ` AND ` + col + ` NOT IN (SELECT key FROM trend_reviews WHERE kind='` + kind + `')`
	}
	return len(app.trendTags(50, 0, unreviewed("tag", "t.tag"))) + len(app.trendLinks(50, 0, unreviewed("link", "s.card_url")))
}

// GET and POST /admin/trends
func (app *App) handleAdminTrends(w http.ResponseWriter, r *http.Request, p *Page) {
	if r.Method == http.MethodPost {
		if r.FormValue("set_review") != "" {
			on := "0"
			if r.FormValue("review") != "" {
				on = "1"
			}
			app.setSetting("trends_review", on)
		} else {
			app.reviewTrend(p.Me, r.FormValue("kind"), r.FormValue("key"), r.FormValue("action"))
		}
		http.Redirect(w, r, "/admin/trends#"+r.FormValue("kind"), http.StatusSeeOther)
		return
	}
	p.Title, p.Section = "Trends", "admin"
	p.TrendTags = app.trendTags(40, 0, "")
	p.TrendLinks = app.trendLinks(20, 0, "")
	p.Statuses = app.trendStatuses(p.Me, 20, 0, "")
	p.TrendStates = app.trendStates()
	app.render(w, "admintrends", p)
}
