package server

import (
	"net/http"
	"strings"
	"time"
)

// The browsers logged in to an account (Settings → Logged-in browsers), like
// Mastodon's session list: when each one signed in and was last used, and a
// way to log out the others. No IP addresses are kept.

type WebSession struct {
	ID                string // sha256 of the cookie; only shown to its owner
	Browser           string
	Created, LastUsed time.Time
	Current           bool
}

func (app *App) webSessions(accountID, current string) []WebSession {
	rows, err := app.db.Query(`SELECT id, user_agent, created_at, last_used FROM sessions WHERE account_id=? AND expires_at > ?
		ORDER BY MAX(created_at, last_used) DESC`, accountID, time.Now().Unix())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []WebSession
	for rows.Next() {
		var s WebSession
		var ua string
		var created, used int64
		rows.Scan(&s.ID, &ua, &created, &used)
		s.Browser, s.Created, s.Current = browserName(ua), time.Unix(created, 0), s.ID == current
		s.LastUsed = s.Created
		if used > created {
			s.LastUsed = time.Unix(used, 0)
		}
		out = append(out, s)
	}
	return out
}

// browserName turns a user agent into "Firefox on Linux".
func browserName(ua string) string {
	has := func(s string) bool { return strings.Contains(ua, s) }
	browser := "Unknown browser"
	switch {
	case has("Firefox/"):
		browser = "Firefox"
	case has("Edg/"):
		browser = "Edge"
	case has("OPR/"):
		browser = "Opera"
	case has("Vivaldi/"):
		browser = "Vivaldi"
	case has("Chrome/") || has("Chromium/"):
		browser = "Chrome"
	case has("Safari/"):
		browser = "Safari"
	}
	switch {
	case has("Android"):
		return browser + " on Android"
	case has("iPhone") || has("iPad"):
		return browser + " on iOS"
	case has("Windows"):
		return browser + " on Windows"
	case has("CrOS"):
		return browser + " on ChromeOS"
	case has("Mac OS X"):
		return browser + " on macOS"
	case has("Linux"):
		return browser + " on Linux"
	}
	return browser
}

func (app *App) handleSessionRevoke(w http.ResponseWriter, r *http.Request, p *Page) {
	if id := r.PathValue("id"); id == "others" {
		app.db.Exec(`DELETE FROM sessions WHERE account_id=? AND id != ?`, p.Me.ID, p.sessID)
		setFlash(w, "All other browsers are logged out.")
	} else if id != p.sessID {
		app.db.Exec(`DELETE FROM sessions WHERE account_id=? AND id=?`, p.Me.ID, id)
		setFlash(w, "That browser is logged out.")
	}
	http.Redirect(w, r, "/settings#sessions", http.StatusSeeOther)
}
