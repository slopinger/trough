package server

import (
	"html/template"
	"net/http"
	"strings"
	"time"
	"unicode"

	"git.freifick.net/slop/trough/internal/ulid"
)

// Server rules and announcements, both written by admins on /admin. Rules
// show on the About and sign-up pages and in apps; announcements show as a
// banner until each user dismisses them, and apps can react to them.

const (
	maxRules           = 30
	maxAnnouncementLen = 2000
)

// Rule is one of the server's rules.
type Rule struct {
	ID, Text, Hint string
}

// Announcement is an admin announcement. Content is HTML made from Text.
type Announcement struct {
	ID, Text, Content string
	StartsAt, EndsAt  time.Time // zero: no limit
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Read              bool // for the viewer
}

// HTML is the announcement's content, sanitized when it was saved.
func (a *Announcement) HTML() template.HTML { return template.HTML(a.Content) }

// Active reports whether a is shown now.
func (a *Announcement) Active() bool {
	now := time.Now()
	return (a.StartsAt.IsZero() || !a.StartsAt.After(now)) && (a.EndsAt.IsZero() || a.EndsAt.After(now))
}

func (app *App) rules() []Rule {
	rows, err := app.db.Query(`SELECT id, text, hint FROM rules ORDER BY position, id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		rows.Scan(&r.ID, &r.Text, &r.Hint)
		out = append(out, r)
	}
	return out
}

func (app *App) addRule(text, hint string) error {
	text, hint = truncateRunes(strings.TrimSpace(text), 300), truncateRunes(strings.TrimSpace(hint), 500)
	if text == "" || len(app.rules()) >= maxRules {
		return nil
	}
	_, err := app.db.Exec(`INSERT INTO rules (id, text, hint, position) VALUES (?,?,?,(SELECT COALESCE(MAX(position),0)+1 FROM rules))`,
		ulid.New(), text, hint)
	return err
}

// announcements returns announcements (only active ones unless all), newest
// first, with the read state for viewer (may be nil).
func (app *App) announcements(viewer *Account, all bool) []*Announcement {
	rows, err := app.db.Query(`SELECT id, text, content, starts_at, ends_at, created_at, updated_at FROM announcements ORDER BY created_at DESC`)
	if err != nil {
		return nil
	}
	var out []*Announcement
	for rows.Next() {
		a := &Announcement{}
		var starts, ends, created, updated int64
		rows.Scan(&a.ID, &a.Text, &a.Content, &starts, &ends, &created, &updated)
		a.StartsAt, a.EndsAt = unixOrZero(starts), unixOrZero(ends)
		a.CreatedAt, a.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
		if all || a.Active() {
			out = append(out, a)
		}
	}
	rows.Close()
	if viewer != nil {
		for _, a := range out {
			a.Read = app.db.QueryRow(`SELECT 1 FROM announcement_reads WHERE announcement_id=? AND account_id=?`, a.ID, viewer.ID).Scan(new(int)) == nil
		}
	}
	return out
}

func unixOrZero(t int64) time.Time {
	if t == 0 {
		return time.Time{}
	}
	return time.Unix(t, 0)
}

func (app *App) unreadAnnouncements(viewer *Account) []*Announcement {
	var out []*Announcement
	for _, a := range app.announcements(viewer, false) {
		if !a.Read {
			out = append(out, a)
		}
	}
	return out
}

func (app *App) addAnnouncement(text string, ends time.Time) error {
	text = truncateRunes(strings.TrimSpace(text), maxAnnouncementLen)
	if text == "" {
		return nil
	}
	now := time.Now().Unix()
	var endsAt int64
	if !ends.IsZero() {
		endsAt = ends.Unix()
	}
	_, err := app.db.Exec(`INSERT INTO announcements (id, text, content, starts_at, ends_at, created_at, updated_at) VALUES (?,?,?,0,?,?,?)`,
		ulid.New(), text, sanitizeHTML(markdownToHTML(text)), endsAt, now, now)
	return err
}

func (app *App) dismissAnnouncement(me *Account, id string) {
	app.db.Exec(`INSERT OR IGNORE INTO announcement_reads (announcement_id, account_id) SELECT id, ? FROM announcements WHERE id=?`, me.ID, id)
}

// validReaction accepts a custom emoji's shortcode or a short Unicode emoji.
func (app *App) validReaction(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	if app.db.QueryRow(`SELECT 1 FROM custom_emojis WHERE shortcode=?`, name).Scan(new(int)) == nil {
		return true
	}
	for _, r := range name {
		if r < 0x80 || unicode.IsLetter(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func (app *App) apiAnnouncement(v *Account, a *Announcement) obj {
	ts := func(t time.Time) any {
		if t.IsZero() {
			return nil
		}
		return apiTime(t)
	}
	reactions := []any{}
	rows, err := app.db.Query(`SELECT r.name, COUNT(*), SUM(r.account_id=?), COALESCE(e.url,'') FROM announcement_reactions r
		LEFT JOIN custom_emojis e ON e.shortcode=r.name WHERE r.announcement_id=? GROUP BY r.name ORDER BY MIN(r.created_at)`, accountIDOf(v), a.ID)
	if err == nil {
		for rows.Next() {
			var name, url string
			var n, mine int
			rows.Scan(&name, &n, &mine, &url)
			m := obj{"name": name, "count": n, "me": mine > 0}
			if url != "" {
				m["url"], m["static_url"] = app.absMedia(url), app.absMedia(url)
			}
			reactions = append(reactions, m)
		}
		rows.Close()
	}
	return obj{"id": a.ID, "content": a.Content, "starts_at": ts(a.StartsAt), "ends_at": ts(a.EndsAt), "all_day": false,
		"published_at": ts(a.CreatedAt), "updated_at": ts(a.UpdatedAt), "read": a.Read, "mentions": []any{}, "statuses": []any{},
		"tags": []any{}, "emojis": []any{}, "reactions": reactions}
}

func accountIDOf(a *Account) string {
	if a == nil {
		return ""
	}
	return a.ID
}

func (app *App) apiRules(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	apiJSON(w, 200, app.apiRuleList())
}

func (app *App) apiRuleList() []any {
	out := []any{}
	for _, r := range app.rules() {
		out = append(out, obj{"id": r.ID, "text": r.Text, "hint": r.Hint})
	}
	return out
}

func (app *App) apiAnnouncements(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	for _, a := range app.announcements(c.Me, false) {
		if r.URL.Query().Get("with_dismissed") == "false" && a.Read {
			continue
		}
		out = append(out, app.apiAnnouncement(c.Me, a))
	}
	apiJSON(w, 200, out)
}

func (app *App) apiDismissAnnouncement(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	app.dismissAnnouncement(c.Me, r.PathValue("id"))
	apiJSON(w, 200, obj{})
}

func (app *App) apiAnnouncementReaction(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	id, name := r.PathValue("id"), r.PathValue("name")
	if app.db.QueryRow(`SELECT 1 FROM announcements WHERE id=?`, id).Scan(new(int)) != nil {
		apiError(w, 404, "Record not found")
		return
	}
	if r.Method == http.MethodDelete {
		app.db.Exec(`DELETE FROM announcement_reactions WHERE announcement_id=? AND account_id=? AND name=?`, id, c.Me.ID, name)
	} else {
		if !app.validReaction(name) {
			apiError(w, 422, "Validation failed: Name is not a recognized emoji")
			return
		}
		app.db.Exec(`INSERT OR IGNORE INTO announcement_reactions (announcement_id, account_id, name, created_at) VALUES (?,?,?,?)`,
			id, c.Me.ID, name, time.Now().Unix())
	}
	apiJSON(w, 200, obj{})
}

// ---- web UI ----

func (app *App) handleDismissAnnouncement(w http.ResponseWriter, r *http.Request, p *Page) {
	app.dismissAnnouncement(p.Me, r.PathValue("id"))
	app.back(w, r, "/")
}

func (app *App) handleAdminRules(w http.ResponseWriter, r *http.Request, p *Page) {
	if id := r.FormValue("delete"); id != "" {
		app.db.Exec(`DELETE FROM rules WHERE id=?`, id)
		setFlash(w, "Rule removed.")
	} else if err := app.addRule(r.FormValue("text"), r.FormValue("hint")); err == nil {
		setFlash(w, "Rules saved.")
	}
	http.Redirect(w, r, "/admin#rules", http.StatusSeeOther)
}

func (app *App) handleAdminAnnouncements(w http.ResponseWriter, r *http.Request, p *Page) {
	if id := r.FormValue("delete"); id != "" {
		app.db.Exec(`DELETE FROM announcements WHERE id=?`, id)
		setFlash(w, "Announcement removed.")
	} else {
		var ends time.Time
		if d, err := time.Parse("2006-01-02", r.FormValue("ends")); err == nil {
			ends = d.Add(24 * time.Hour)
		}
		app.addAnnouncement(r.FormValue("text"), ends)
		setFlash(w, "Announcement published.")
	}
	http.Redirect(w, r, "/admin#announcements", http.StatusSeeOther)
}
