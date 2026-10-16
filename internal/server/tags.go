package server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ---- followed hashtags ----

func normalizeTag(t string) string {
	t = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "#")))
	if t == "" || len(t) > 100 || strings.ContainsAny(t, " #/?&") {
		return ""
	}
	return t
}

func (app *App) FollowTag(me *Account, tag string) error {
	if tag = normalizeTag(tag); tag == "" {
		return errors.New("invalid hashtag")
	}
	_, err := app.db.Exec(`INSERT OR IGNORE INTO followed_tags (account_id, tag, created_at) VALUES (?,?,?)`, me.ID, tag, time.Now().Unix())
	return err
}

func (app *App) UnfollowTag(me *Account, tag string) error {
	_, err := app.db.Exec(`DELETE FROM followed_tags WHERE account_id=? AND tag=?`, me.ID, normalizeTag(tag))
	return err
}

func (app *App) followsTag(me *Account, tag string) bool {
	var x int
	return me != nil && app.db.QueryRow(`SELECT 1 FROM followed_tags WHERE account_id=? AND tag=?`, me.ID, normalizeTag(tag)).Scan(&x) == nil
}

func (app *App) followedTags(me *Account) []string {
	rows, err := app.db.Query(`SELECT tag FROM followed_tags WHERE account_id=? ORDER BY tag`, me.ID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		rows.Scan(&t)
		out = append(out, t)
	}
	return out
}

// homeSQL is what belongs in a user's home timeline: their own posts, posts
// by people they follow, mentions, public posts with followed hashtags, but
// nothing from members of exclusive lists or muted accounts.
func homeSQL(v *Account) (string, []any) {
	mute, margs := muteSQL(v)
	return `(s.account_id=? OR s.account_id IN (SELECT target_id FROM follows WHERE account_id=? AND accepted=1)
		OR EXISTS(SELECT 1 FROM mentions m2 WHERE m2.status_id=s.id AND m2.account_id=?)
		OR (s.visibility='public' AND s.id IN (SELECT status_id FROM status_tags WHERE tag IN (SELECT tag FROM followed_tags WHERE account_id=?))))
		AND (s.account_id=? OR s.account_id NOT IN (SELECT la.account_id FROM list_accounts la JOIN lists l ON l.id=la.list_id WHERE l.account_id=? AND l.exclusive=1))
		AND ` + mute, append([]any{v.ID, v.ID, v.ID, v.ID, v.ID, v.ID}, margs...)
}

func (app *App) handleTagFollow(w http.ResponseWriter, r *http.Request, p *Page) {
	tag := r.PathValue("tag")
	var err error
	if r.PathValue("action") == "unfollow" {
		err = app.UnfollowTag(p.Me, tag)
	} else {
		err = app.FollowTag(p.Me, tag)
	}
	if err != nil {
		setFlash(w, err.Error())
	}
	app.back(w, r, "/tags/"+url.PathEscape(normalizeTag(tag)))
}

func (app *App) apiTag(name string, following bool) obj {
	return obj{"name": name, "url": app.cfg.BaseURL() + "/tags/" + name, "history": apiHistory(app.tagHistory(name)), "following": following}
}

func (app *App) apiFollowedTags(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	for _, t := range app.followedTags(c.Me) {
		out = append(out, app.apiTag(t, true))
	}
	apiJSON(w, 200, out)
}

func (app *App) apiGetTag(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	t := normalizeTag(r.PathValue("name"))
	if t == "" {
		apiError(w, 404, "Record not found")
		return
	}
	apiJSON(w, 200, app.apiTagFor(c.Me, t))
}

func (app *App) apiTagAction(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	t := normalizeTag(r.PathValue("name"))
	var err error
	if r.PathValue("action") == "unfollow" {
		err = app.UnfollowTag(c.Me, t)
	} else {
		err = app.FollowTag(c.Me, t)
	}
	if err != nil {
		apiError(w, 422, err.Error())
		return
	}
	apiJSON(w, 200, app.apiTag(t, r.PathValue("action") != "unfollow"))
}

// FollowedTag is a row on the followed hashtags page.
type FollowedTag struct {
	Name       string
	History    []TagDay
	LastUsed   time.Time
	FollowedAt time.Time
	Featured   bool
}

func (app *App) followedTagRows(me *Account) []FollowedTag {
	rows, err := app.db.Query(`SELECT f.tag, f.created_at,
		(SELECT COALESCE(MAX(s.created_at),0) FROM status_tags t JOIN statuses s ON s.id=t.status_id WHERE t.tag=f.tag AND s.visibility='public'),
		EXISTS(SELECT 1 FROM featured_tags ft WHERE ft.account_id=f.account_id AND ft.tag=f.tag)
		FROM followed_tags f WHERE f.account_id=? ORDER BY f.tag`, me.ID)
	if err != nil {
		return nil
	}
	var out []FollowedTag
	for rows.Next() {
		var t FollowedTag
		var at, last int64
		rows.Scan(&t.Name, &at, &last, &t.Featured)
		t.FollowedAt = time.Unix(at, 0)
		if last > 0 {
			t.LastUsed = time.Unix(last, 0)
		}
		out = append(out, t)
	}
	rows.Close()
	for i := range out {
		out[i].History = app.tagHistory(out[i].Name)
	}
	return out
}

func (app *App) handleFollowedTags(w http.ResponseWriter, r *http.Request, p *Page) {
	if r.Method == http.MethodPost {
		tag := normalizeTag(r.FormValue("tag"))
		if err := app.FollowTag(p.Me, tag); err != nil {
			setFlash(w, "That isn't a hashtag.")
		} else {
			setFlash(w, "Following #"+tag+". Public posts with it now show up in Home.")
		}
		http.Redirect(w, r, "/followed_tags", http.StatusSeeOther)
		return
	}
	p.Title, p.Section = "Followed hashtags", "tags"
	p.FollowedTagRows = app.followedTagRows(p.Me)
	app.render(w, "followedtags", p)
}
