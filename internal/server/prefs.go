package server

import (
	"net/http"
	"slices"
)

// Posting and reading preferences (Settings → Preferences), like Mastodon's:
// defaults for new posts, and how sensitive media and content warnings show.

type Prefs struct {
	Visibility string // default for new posts
	Sensitive  bool   // always mark media as sensitive
	Language   string // default post language; "" uses the last one used
	Media      string // "default" (hide sensitive), "show_all" or "hide_all"
	Spoilers   bool   // expand posts with content warnings
}

var mediaPrefs = []string{"default", "show_all", "hide_all"}

func (app *App) prefs(accountID string) Prefs {
	pr := Prefs{Visibility: "public", Media: "default"}
	var sensitive, spoilers int
	app.db.QueryRow(`SELECT default_visibility, default_sensitive, default_language, expand_media, expand_spoilers FROM users WHERE account_id=?`,
		accountID).Scan(&pr.Visibility, &sensitive, &pr.Language, &pr.Media, &spoilers)
	pr.Sensitive, pr.Spoilers = sensitive == 1, spoilers == 1
	return pr
}

func (app *App) setPrefs(accountID string, pr Prefs) {
	if _, ok := visibilityRank[pr.Visibility]; !ok {
		pr.Visibility = "public"
	}
	if !slices.Contains(mediaPrefs, pr.Media) {
		pr.Media = "default"
	}
	if !knownLanguage(pr.Language) {
		pr.Language = ""
	}
	app.db.Exec(`UPDATE users SET default_visibility=?, default_sensitive=?, default_language=?, expand_media=?, expand_spoilers=? WHERE account_id=?`,
		pr.Visibility, pr.Sensitive, pr.Language, pr.Media, pr.Spoilers, accountID)
}

func knownLanguage(code string) bool {
	return slices.ContainsFunc(postLanguages, func(l struct{ Code, Name string }) bool { return l.Code == code })
}

// composeDefaults fills a fresh compose box (Visibility not chosen yet) from
// the preferences. A reply keeps the stricter of the default and its parent's
// visibility.
func composeDefaults(c *Compose, pr Prefs) {
	if c == nil || c.EditOf != nil || c.Visibility != "" {
		return
	}
	c.Visibility = pr.Visibility
	if c.ReplyTo != nil && visibilityRank[c.ReplyTo.Visibility] > visibilityRank[c.Visibility] {
		c.Visibility = c.ReplyTo.Visibility
	}
	c.Sensitive = c.Sensitive || pr.Sensitive
	if c.Language == "" {
		c.Language = pr.Language
	}
}

// hideMedia reports whether a post's media start out hidden behind a click.
func (pr Prefs) hideMedia(s *Status) bool {
	switch {
	case s.Blurred || pr.Media == "hide_all":
		return true
	case pr.Media == "show_all":
		return false
	}
	return s.Sensitive
}

func (app *App) handlePreferences(w http.ResponseWriter, r *http.Request, p *Page) {
	app.setPrefs(p.Me.ID, Prefs{
		Visibility: r.FormValue("visibility"),
		Sensitive:  r.FormValue("sensitive") != "",
		Language:   r.FormValue("language"),
		Media:      r.FormValue("media"),
		Spoilers:   r.FormValue("spoilers") != "",
	})
	setFlash(w, "Preferences saved.")
	http.Redirect(w, r, "/settings#preferences", http.StatusSeeOther)
}
