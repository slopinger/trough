package server

import (
	"net/http"
	"strings"
	"time"
)

func (app *App) handleLoginForm(w http.ResponseWriter, r *http.Request, p *Page) {
	if p.Me != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	p.Title = "Log in"
	p.Query = r.URL.Query().Get("next")
	p.Registrations = app.registrationMode()
	app.render(w, "login", p)
}

func (app *App) handleLogin(w http.ResponseWriter, r *http.Request, p *Page) {
	ip := clientIP(r)
	p.Title = "Log in"
	p.Query = r.FormValue("next")
	if app.logins.blocked(ip) {
		p.Error = "Too many failed attempts. Try again in a few minutes."
		app.renderStatus(w, http.StatusTooManyRequests, "login", p)
		return
	}
	acc, err := app.checkPassword(strings.TrimPrefix(strings.TrimSpace(r.FormValue("username")), "@"), r.FormValue("password"))
	if err != nil {
		app.logins.hit(ip)
		p.Error = "Invalid username or password."
		app.renderStatus(w, http.StatusUnauthorized, "login", p)
		return
	}
	next := r.FormValue("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		next = "/"
	}
	if app.twoFactorEnabled(acc.ID) {
		startPendingLogin(w, !app.cfg.Insecure, acc.ID, next)
		app.render(w, "login_code", p)
		return
	}
	if err := app.startSession(w, r, acc); err != nil {
		http.Error(w, "could not create session", 500)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (app *App) handleLogout(w http.ResponseWriter, r *http.Request, p *Page) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		app.db.Exec(`DELETE FROM sessions WHERE id=?`, sha256Hex(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (app *App) startSession(w http.ResponseWriter, r *http.Request, acc *Account) error {
	token := randomToken(32)
	now := time.Now()
	if _, err := app.db.Exec(`INSERT INTO sessions (id,account_id,csrf,created_at,expires_at,user_agent) VALUES (?,?,?,?,?,?)`,
		sha256Hex(token), acc.ID, randomToken(16), now.Unix(), now.Add(sessionTTL).Unix(), truncateRunes(r.UserAgent(), 200)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: !app.cfg.Insecure,
		SameSite: http.SameSiteLaxMode, Expires: now.Add(sessionTTL),
	})
	return nil
}
