package main

import (
	"net/http"
	"strings"
	"time"
)

func (app *App) requireAdmin(h handler) handler {
	return func(w http.ResponseWriter, r *http.Request, p *Page) {
		if !p.Admin {
			http.Error(w, "admins only", http.StatusForbidden)
			return
		}
		h(w, r, p)
	}
}

func (app *App) domainBlockList() []DomainBlock {
	rows, err := app.db.Query(`SELECT domain, reason, created_at FROM domain_blocks ORDER BY domain`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []DomainBlock
	for rows.Next() {
		var d DomainBlock
		var t int64
		rows.Scan(&d.Domain, &d.Reason, &t)
		d.CreatedAt = time.Unix(t, 0)
		out = append(out, d)
	}
	return out
}

func (app *App) handleAdmin(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section = "Admin", "admin"
	p.Users, _ = app.ListUsers()
	p.DomainBlocks = app.domainBlockList()
	p.Description = app.getSetting("description")
	p.Stats = map[string]int{
		"users":     app.countInt(`SELECT COUNT(*) FROM users`),
		"local":     app.countInt(`SELECT COUNT(*) FROM statuses WHERE local=1 AND boost_of_id=''`),
		"remote":    app.countInt(`SELECT COUNT(*) FROM statuses WHERE local=0`),
		"accounts":  app.countInt(`SELECT COUNT(*) FROM accounts WHERE domain != ''`),
		"instances": app.countInt(`SELECT COUNT(DISTINCT domain) FROM accounts WHERE domain != ''`),
		"queued":    app.countInt(`SELECT COUNT(*) FROM deliveries`),
		"retrying":  app.countInt(`SELECT COUNT(*) FROM deliveries WHERE attempts > 0`),
	}
	rows, err := app.db.Query(`SELECT inbox, attempts, next_try, last_error FROM deliveries WHERE attempts > 0 ORDER BY next_try LIMIT 20`)
	if err == nil {
		for rows.Next() {
			var d DeliveryInfo
			var t int64
			rows.Scan(&d.Inbox, &d.Attempts, &t, &d.LastError)
			d.NextTry = time.Unix(t, 0)
			p.Deliveries = append(p.Deliveries, d)
		}
		rows.Close()
	}
	app.render(w, "admin", p)
}

func (app *App) handleAdminCreateUser(w http.ResponseWriter, r *http.Request, p *Page) {
	pw := randomPassword()
	acc, err := app.CreateLocalUser(strings.TrimSpace(r.FormValue("username")), pw, r.FormValue("admin") != "")
	if err != nil {
		setFlash(w, "Could not create user: "+err.Error())
	} else {
		setFlash(w, "Created @"+acc.Username+" with password: "+pw)
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (app *App) handleAdminResetPassword(w http.ResponseWriter, r *http.Request, p *Page) {
	acc, err := app.AccountByID(r.PathValue("id"))
	if err != nil || !acc.IsLocal() {
		http.NotFound(w, r)
		return
	}
	pw := randomPassword()
	if err := app.SetPassword(acc.ID, pw); err != nil {
		setFlash(w, err.Error())
	} else {
		setFlash(w, "New password for @"+acc.Username+": "+pw)
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (app *App) handleAdminSettings(w http.ResponseWriter, r *http.Request, p *Page) {
	app.setSetting("title", truncateRunes(strings.TrimSpace(r.FormValue("title")), 100))
	app.setSetting("description", truncateRunes(strings.TrimSpace(r.FormValue("description")), 2000))
	setFlash(w, "Instance settings saved.")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (app *App) handleAdminDomainBlock(w http.ResponseWriter, r *http.Request, p *Page) {
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	domain = strings.TrimPrefix(strings.TrimPrefix(domain, "https://"), "http://")
	domain = strings.Trim(domain, "/ ")
	if r.FormValue("remove") != "" {
		app.db.Exec(`DELETE FROM domain_blocks WHERE domain=?`, domain)
		app.db.Exec(`UPDATE accounts SET suspended=0 WHERE domain=? OR domain LIKE ?`, domain, "%."+domain)
		setFlash(w, "Unblocked "+domain+".")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if domain == "" || strings.ContainsAny(domain, " /@") || domain == app.cfg.Host {
		setFlash(w, "Invalid domain.")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	app.db.Exec(`INSERT INTO domain_blocks (domain, reason, created_at) VALUES (?,?,?) ON CONFLICT(domain) DO UPDATE SET reason=excluded.reason`,
		domain, truncateRunes(r.FormValue("reason"), 500), time.Now().Unix())
	match := []any{domain, "%." + domain}
	app.db.Exec(`UPDATE accounts SET suspended=1 WHERE domain=? OR domain LIKE ?`, match...)
	app.db.Exec(`DELETE FROM follows WHERE account_id IN (SELECT id FROM accounts WHERE domain=? OR domain LIKE ?)
		OR target_id IN (SELECT id FROM accounts WHERE domain=? OR domain LIKE ?)`, domain, "%."+domain, domain, "%."+domain)
	app.db.Exec(`DELETE FROM deliveries WHERE inbox LIKE ? OR inbox LIKE ?`, "https://"+domain+"/%", "https://%."+domain+"/%")
	setFlash(w, "Blocked "+domain+".")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
