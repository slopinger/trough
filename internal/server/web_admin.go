package server

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
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

// ServerCount is a remote server and how many of its posts arrived.
type ServerCount struct {
	Domain string
	Posts  int
}

// weekStats adds the admin page's last-7-days numbers, like the top of
// Mastodon's admin dashboard.
func (app *App) weekStats(p *Page) {
	since := time.Now().Add(-7 * 24 * time.Hour).Unix()
	for k, q := range map[string]string{
		"new_users":    `SELECT COUNT(*) FROM users WHERE created_at >= ?`,
		"active_users": `SELECT COUNT(*) FROM users WHERE last_seen >= ?`,
		"week_posts":   `SELECT COUNT(*) FROM statuses WHERE local=1 AND boost_of_id='' AND created_at >= ?`,
		"interactions": `SELECT (SELECT COUNT(*) FROM likes l JOIN statuses s ON s.id=l.status_id WHERE s.local=1 AND l.created_at >= ?1)
			+ (SELECT COUNT(*) FROM statuses b JOIN statuses s ON s.id=b.boost_of_id WHERE s.local=1 AND b.created_at >= ?1)
			+ (SELECT COUNT(*) FROM statuses r JOIN statuses s ON s.id=r.in_reply_to_id WHERE s.local=1 AND r.account_id != s.account_id AND r.created_at >= ?1)`,
		"new_accounts": `SELECT COUNT(*) FROM accounts WHERE domain != '' AND created_at >= ?`,
	} {
		p.Stats[k] = app.countInt(q, since)
	}
	rows, err := app.db.Query(`SELECT a.domain, COUNT(*) n FROM statuses s JOIN accounts a ON a.id=s.account_id
		WHERE s.local=0 AND s.created_at >= ? GROUP BY a.domain ORDER BY n DESC LIMIT 5`, since)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var c ServerCount
		if rows.Scan(&c.Domain, &c.Posts) == nil {
			p.TopServers = append(p.TopServers, c)
		}
	}
}

func (app *App) handleAdmin(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section = "Admin", "admin"
	p.Users, _ = app.ListUsers()
	p.DomainBlocks = app.domainBlockList()
	p.Description = app.getSetting("description")
	p.PolicySource, _, p.PolicyDefault = app.privacyPolicy()
	app.adminTerms(p)
	p.PendingSignups = app.pendingSignups()
	p.LimitedAccounts = app.limitedAccounts()
	if len(p.PendingSignups) > 0 { // they are listed on their own
		users := p.Users[:0]
		for _, u := range p.Users {
			if !app.signupPending(u.Account.ID) {
				users = append(users, u)
			}
		}
		p.Users = users
	}
	p.ActivityCharts = activityCharts(app.weeklyActivity(time.Now()))
	p.AIKeySource, p.AIModel = app.aiKeySource(), app.aiModel()
	p.Reports = app.openReports()
	p.Invites = app.invites()
	p.Relays = app.relays()
	p.CustomEmojis = app.customEmojis()
	p.Registrations = app.registrationMode()
	p.Rules = app.rules()
	p.Announcements = app.announcements(p.Me, true)
	p.Stats = map[string]int{
		"users":     app.countInt(`SELECT COUNT(*) FROM users`),
		"local":     app.countInt(`SELECT COUNT(*) FROM statuses WHERE local=1 AND boost_of_id=''`),
		"remote":    app.countInt(`SELECT COUNT(*) FROM statuses WHERE local=0`),
		"accounts":  app.countInt(`SELECT COUNT(*) FROM accounts WHERE domain != ''`),
		"instances": app.countInt(`SELECT COUNT(DISTINCT domain) FROM accounts WHERE domain != ''`),
		"queued":    app.countInt(`SELECT COUNT(*) FROM deliveries`),
		"retrying":  app.countInt(`SELECT COUNT(*) FROM deliveries WHERE attempts > 0`),
	}
	app.weekStats(p)
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
	pw := RandomPassword()
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
	pw := RandomPassword()
	if err := app.SetPassword(acc.ID, pw); err != nil {
		setFlash(w, err.Error())
	} else {
		setFlash(w, "New password for @"+acc.Username+": "+pw)
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (app *App) handleAdminRegistrations(w http.ResponseWriter, r *http.Request, p *Page) {
	mode := r.FormValue("mode")
	if mode != "open" && mode != "invite" && mode != "approval" {
		mode = "closed"
	}
	app.setSetting("registrations", mode)
	setFlash(w, "Registrations are now "+mode+".")
	http.Redirect(w, r, "/admin#registrations", http.StatusSeeOther)
}

func (app *App) handleAdminInvite(w http.ResponseWriter, r *http.Request, p *Page) {
	uses, _ := strconv.Atoi(r.FormValue("uses"))
	days, _ := strconv.Atoi(r.FormValue("days"))
	code, err := app.CreateInvite(p.Me, uses, time.Duration(days)*24*time.Hour)
	if err != nil {
		setFlash(w, err.Error())
	} else {
		setFlash(w, "Invite link: "+app.cfg.BaseURL()+"/signup?invite="+code)
		if app.registrationMode() == "closed" {
			app.setSetting("registrations", "invite")
		}
	}
	http.Redirect(w, r, "/admin#registrations", http.StatusSeeOther)
}

func (app *App) handleAdminReports(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section = "Reports", "admin"
	if p.Tab = r.URL.Query().Get("tab"); p.Tab == "resolved" {
		p.Reports = app.resolvedReports()
	} else {
		p.Tab, p.Reports = "", app.openReports()
	}
	p.Stats = map[string]int{"open": app.countInt(`SELECT COUNT(*) FROM reports WHERE resolved_at=0`)}
	app.render(w, "reports", p)
}

func (app *App) handleReportAction(w http.ResponseWriter, r *http.Request, p *Page) {
	msg, err := app.ResolveReport(p.Me, r.PathValue("id"), r.FormValue("action"), strings.TrimSpace(r.FormValue("note")))
	if err != nil {
		msg = "Couldn't do that: " + err.Error()
	}
	setFlash(w, msg)
	http.Redirect(w, r, "/admin/reports", http.StatusSeeOther)
}

func (app *App) handleUnsuspend(w http.ResponseWriter, r *http.Request, p *Page) {
	a, err := app.AccountByID(r.PathValue("id"))
	if err == nil {
		err = app.UnsuspendAccount(a)
	}
	switch {
	case err != nil:
		setFlash(w, "Couldn't lift the suspension: "+err.Error())
	case a.IsLocal():
		setFlash(w, "Lifted the suspension of @"+a.Username+". They can log in again, and other servers were told.")
	default:
		setFlash(w, "Lifted the suspension of "+a.Acct()+". Follows that ended stay ended.")
	}
	http.Redirect(w, r, adminBack(r), http.StatusSeeOther)
}

// handleSuspend suspends a local user from the user list on /admin.
func (app *App) handleSuspend(w http.ResponseWriter, r *http.Request, p *Page) {
	a, err := app.AccountByID(r.PathValue("id"))
	if err == nil {
		err = app.AdminSuspend(p.Me, a)
	}
	if err != nil {
		setFlash(w, "Couldn't suspend: "+err.Error())
	} else {
		setFlash(w, "Suspended "+a.Acct()+".")
	}
	http.Redirect(w, r, adminBack(r), http.StatusSeeOther)
}

func adminBack(r *http.Request) string {
	if r.FormValue("back") == "users" {
		return "/admin#users"
	}
	return "/admin/reports?tab=resolved"
}

func (app *App) handleAdminAI(w http.ResponseWriter, r *http.Request, p *Page) {
	if k := strings.TrimSpace(r.FormValue("api_key")); k != "" {
		app.setSetting("ai_api_key", k)
	}
	if r.FormValue("remove_key") != "" {
		app.db.Exec(`DELETE FROM settings WHERE key='ai_api_key'`)
	}
	disabled := "0"
	if r.FormValue("enabled") == "" {
		disabled = "1"
	}
	app.setSetting("ai_disabled", disabled)
	app.setSetting("ai_model", strings.TrimSpace(r.FormValue("model")))
	setFlash(w, "AI settings saved.")
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
	if err := app.BlockServer(domain, r.FormValue("reason")); err != nil {
		setFlash(w, "Invalid domain.")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	setFlash(w, "Blocked "+domain+".")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// BlockServer blocks a server (admin domain block) and its subdomains: their accounts are suspended,
// follows in both directions dropped and pending deliveries to them cancelled.
func (app *App) BlockServer(domain, reason string) error {
	if domain == "" || strings.ContainsAny(domain, " /@") || domain == app.cfg.Host {
		return errors.New("invalid domain")
	}
	app.db.Exec(`INSERT INTO domain_blocks (domain, reason, created_at) VALUES (?,?,?) ON CONFLICT(domain) DO UPDATE SET reason=excluded.reason`,
		domain, truncateRunes(reason, 500), time.Now().Unix())
	match := []any{domain, "%." + domain}
	app.db.Exec(`UPDATE accounts SET suspended=1 WHERE domain=? OR domain LIKE ?`, match...)
	app.db.Exec(`DELETE FROM follows WHERE account_id IN (SELECT id FROM accounts WHERE domain=? OR domain LIKE ?)
		OR target_id IN (SELECT id FROM accounts WHERE domain=? OR domain LIKE ?)`, domain, "%."+domain, domain, "%."+domain)
	app.db.Exec(`DELETE FROM deliveries WHERE inbox LIKE ? OR inbox LIKE ?`, "https://"+domain+"/%", "https://%."+domain+"/%")
	return nil
}

// SetAdmin grants or revokes admin rights of a local account.
func (app *App) SetAdmin(accountID string, admin bool) error {
	_, err := app.db.Exec(`UPDATE users SET admin=? WHERE account_id=?`, admin, accountID)
	return err
}

// RandomPassword returns a new random 20-character password.
func RandomPassword() string {
	b := make([]byte, 15)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
