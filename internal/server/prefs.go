package server

import (
	"net/http"
	"slices"
	"strings"
)

// Posting and reading preferences (Settings → Preferences), like Mastodon's:
// defaults for new posts, and how sensitive media and content warnings show.

type Prefs struct {
	Visibility string   // default for new posts
	Sensitive  bool     // always mark media as sensitive
	Language   string   // default post language; "" uses the last one used
	Media      string   // "default" (hide sensitive), "show_all" or "hide_all"
	Spoilers   bool     // expand posts with content warnings
	Languages  []string // public timelines only show these (and posts without a language); empty: all
}

var mediaPrefs = []string{"default", "show_all", "hide_all"}

func (app *App) prefs(accountID string) Prefs {
	pr := Prefs{Visibility: "public", Media: "default"}
	var sensitive, spoilers int
	var langs string
	app.db.QueryRow(`SELECT default_visibility, default_sensitive, default_language, expand_media, expand_spoilers, chosen_languages FROM users WHERE account_id=?`,
		accountID).Scan(&pr.Visibility, &sensitive, &pr.Language, &pr.Media, &spoilers, &langs)
	pr.Sensitive, pr.Spoilers = sensitive == 1, spoilers == 1
	if langs != "" {
		pr.Languages = strings.Split(langs, ",")
	}
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
	langs := slices.DeleteFunc(slices.Clone(pr.Languages), func(l string) bool { return !knownLanguage(l) })
	slices.Sort(langs)
	app.db.Exec(`UPDATE users SET default_visibility=?, default_sensitive=?, default_language=?, expand_media=?, expand_spoilers=?, chosen_languages=? WHERE account_id=?`,
		pr.Visibility, pr.Sensitive, pr.Language, pr.Media, pr.Spoilers, strings.Join(slices.Compact(langs), ","), accountID)
}

// Chooses reports whether public timelines are limited to code (for the settings form).
func (pr Prefs) Chooses(code string) bool { return slices.Contains(pr.Languages, code) }

// languageSQL keeps public timeline posts in the viewer's chosen languages
// (like Mastodon's "filter languages"), comparing the primary language
// subtag, and always keeps posts without a language.
func (app *App) languageSQL(v *Account) (string, []any) {
	if v == nil {
		return "1", nil
	}
	langs := app.prefs(v.ID).Languages
	if len(langs) == 0 {
		return "1", nil
	}
	return inClause(`(s.language='' OR lower(substr(s.language, 1, instr(s.language || '-', '-') - 1)) IN (%s))`, langs)
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
		Languages:  r.Form["languages"],
	})
	setFlash(w, "Preferences saved.")
	http.Redirect(w, r, "/settings#preferences", http.StatusSeeOther)
}
