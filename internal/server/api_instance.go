package server

import (
	"net/http"
	"time"
)

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
		"reading:expand:media": "default", "reading:expand:spoilers": false, "posting:default:quoted_policy": app.defaultQuotePolicy(c.Me.ID)})
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
