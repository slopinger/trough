package server

import (
	"net/http"
)

func (app *App) handleFollowRequests(w http.ResponseWriter, r *http.Request, p *Page) {
	list, err := app.followList(p.Me, true, true, 0)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Section = "Follow requests", "requests"
	p.Accounts = list
	app.render(w, "requests", p)
}

func (app *App) handleNotifications(w http.ResponseWriter, r *http.Request, p *Page) {
	ns, err := app.notifications(p.Me, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Section = "Notifications", "notifications"
	p.Notifications = groupNotifications(ns)
	p.PendingRequests, _ = app.pendingNotificationCounts(p.Me)
	if len(ns) == pageSize*2 {
		p.NextURL = "/notifications?max_id=" + ns[len(ns)-1].ID
	}
	if r.URL.Query().Get("max_id") == "" {
		app.db.Exec(`UPDATE notifications SET read=1 WHERE account_id=? AND read=0`, p.Me.ID)
	}
	app.render(w, "notifications", p)
}

func (app *App) handleClearNotifications(w http.ResponseWriter, r *http.Request, p *Page) {
	app.db.Exec(`DELETE FROM notifications WHERE account_id=?`, p.Me.ID)
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}
