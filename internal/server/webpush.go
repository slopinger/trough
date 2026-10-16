package server

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"git.freifick.net/slop/trough/internal/ulid"
	"git.freifick.net/slop/trough/web"
)

// Browser notifications for the web UI: the page subscribes through the
// browser's push service (with the same VAPID key apps use) and a service
// worker at /sw.js shows the notifications, opening the post on click.

// webPushTarget is a browser subscribed from the web UI.
type webPushTarget struct{ id, endpoint, p256dh, auth string }

func (app *App) webPushTargets(accountID string) []webPushTarget {
	rows, err := app.db.Query(`SELECT id, endpoint, p256dh, auth FROM web_push_subscriptions WHERE account_id=?`, accountID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []webPushTarget
	for rows.Next() {
		var t webPushTarget
		rows.Scan(&t.id, &t.endpoint, &t.p256dh, &t.auth)
		out = append(out, t)
	}
	return out
}

// validPushEndpoint accepts https URLs (http too in dev mode), like the API.
func (app *App) validPushEndpoint(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || app.cfg.Insecure && u.Scheme == "http") && u.Host != "" && len(s) < 1000
}

func (app *App) handleWebPush(w http.ResponseWriter, r *http.Request, p *Page) {
	endpoint := r.FormValue("endpoint")
	if r.FormValue("action") == "unsubscribe" {
		app.db.Exec(`DELETE FROM web_push_subscriptions WHERE account_id=? AND endpoint=?`, p.Me.ID, endpoint)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p256dh, auth := r.FormValue("p256dh"), r.FormValue("auth")
	if !app.validPushEndpoint(endpoint) || p256dh == "" || auth == "" || len(p256dh) > 200 || len(auth) > 100 {
		http.Error(w, "bad subscription", http.StatusBadRequest)
		return
	}
	if app.countInt(`SELECT COUNT(*) FROM web_push_subscriptions WHERE account_id=?`, p.Me.ID) >= 10 {
		app.db.Exec(`DELETE FROM web_push_subscriptions WHERE id=(SELECT id FROM web_push_subscriptions WHERE account_id=? ORDER BY created_at LIMIT 1)`, p.Me.ID)
	}
	app.db.Exec(`INSERT INTO web_push_subscriptions (id, account_id, endpoint, p256dh, auth, created_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(endpoint) DO UPDATE SET account_id=excluded.account_id, p256dh=excluded.p256dh, auth=excluded.auth`,
		ulid.New(), p.Me.ID, endpoint, p256dh, auth, time.Now().Unix())
	w.WriteHeader(http.StatusNoContent)
}

// handleServiceWorker serves /sw.js from the root, so it may show
// notifications for every page of the site.
func (app *App) handleServiceWorker(w http.ResponseWriter, r *http.Request) {
	b, err := web.Static.ReadFile("static/sw.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

// notificationURL is the page a notification opens.
func (app *App) notificationURL(n *Notification) string {
	if n.Status != nil && !strings.HasPrefix(n.Type, "follow") {
		return n.Status.Path()
	}
	return "/notifications"
}
