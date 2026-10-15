package server

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"git.freifick.net/slop/trough/internal/ap"
	"git.freifick.net/slop/trough/internal/ulid"
)

// Featured hashtags: up to maxFeaturedTags hashtags a user shows on their
// profile, each linking to their posts with it (/@name/tagged/tag). As on
// Mastodon they are published in the actor's featuredTags collection.

const maxFeaturedTags = 10

var (
	errInvalidTag      = errors.New("not a valid hashtag")
	errTooManyFeatured = errors.New("you can feature at most 10 hashtags")
)

// actorUpdated tells followers' servers that me's actor document changed.
func (app *App) actorUpdated(me *Account) {
	app.enqueue(me, app.followerInboxes(me.ID), app.updateActorActivity(me))
}

// FeaturedTag is a hashtag featured on a profile, with the account's use of it.
type FeaturedTag struct {
	ID, Name      string
	StatusesCount int
	LastStatusAt  time.Time
}

func (app *App) featuredTags(acc *Account) []FeaturedTag {
	rows, err := app.db.Query(`SELECT f.id, f.tag,
		(SELECT COUNT(*) FROM status_tags t JOIN statuses s ON s.id=t.status_id WHERE t.tag=f.tag AND s.account_id=f.account_id AND s.visibility IN ('public','unlisted')),
		(SELECT COALESCE(MAX(s.created_at),0) FROM status_tags t JOIN statuses s ON s.id=t.status_id WHERE t.tag=f.tag AND s.account_id=f.account_id AND s.visibility IN ('public','unlisted'))
		FROM featured_tags f WHERE f.account_id=? ORDER BY f.created_at`, acc.ID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []FeaturedTag
	for rows.Next() {
		var f FeaturedTag
		var last int64
		rows.Scan(&f.ID, &f.Name, &f.StatusesCount, &last)
		if last > 0 {
			f.LastStatusAt = time.Unix(last, 0)
		}
		out = append(out, f)
	}
	return out
}

func (app *App) FeatureTag(me *Account, name string) (*FeaturedTag, error) {
	tag := normalizeTag(name)
	if tag == "" {
		return nil, errInvalidTag
	}
	if app.countInt(`SELECT COUNT(*) FROM featured_tags WHERE account_id=?`, me.ID) >= maxFeaturedTags {
		return nil, errTooManyFeatured
	}
	app.db.Exec(`INSERT OR IGNORE INTO featured_tags (id, account_id, tag, created_at) VALUES (?,?,?,?)`, ulid.New(), me.ID, tag, time.Now().Unix())
	for _, f := range app.featuredTags(me) {
		if f.Name == tag {
			app.actorUpdated(me)
			return &f, nil
		}
	}
	return nil, errNotFound
}

func (app *App) UnfeatureTag(me *Account, idOrName string) error {
	res, err := app.db.Exec(`DELETE FROM featured_tags WHERE account_id=? AND (id=? OR tag=?)`, me.ID, idOrName, normalizeTag(idOrName))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	app.actorUpdated(me)
	return nil
}

func (app *App) featuresTag(me *Account, tag string) bool {
	return app.db.QueryRow(`SELECT 1 FROM featured_tags WHERE account_id=? AND tag=?`, me.ID, tag).Scan(new(int)) == nil
}

// tagSuggestions are the hashtags me used most in the last weeks and doesn't feature yet.
func (app *App) tagSuggestions(me *Account) []string {
	rows, err := app.db.Query(`SELECT t.tag FROM status_tags t JOIN statuses s ON s.id=t.status_id
		WHERE s.account_id=? AND s.created_at > ? AND t.tag NOT IN (SELECT tag FROM featured_tags WHERE account_id=?)
		GROUP BY t.tag ORDER BY COUNT(*) DESC LIMIT 10`, me.ID, time.Now().AddDate(0, 0, -30).Unix(), me.ID)
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

func (app *App) taggedURL(acc *Account, tag string) string {
	return app.cfg.BaseURL() + acc.Path() + "/tagged/" + url.PathEscape(tag)
}

func (app *App) apiFeaturedTag(acc *Account, f FeaturedTag) obj {
	var last any
	if !f.LastStatusAt.IsZero() {
		last = f.LastStatusAt.UTC().Format("2006-01-02")
	}
	return obj{"id": f.ID, "name": f.Name, "url": app.taggedURL(acc, f.Name), "statuses_count": strconv.Itoa(f.StatusesCount), "last_status_at": last}
}

func (app *App) writeFeaturedTags(w http.ResponseWriter, acc *Account) {
	out := []any{}
	for _, f := range app.featuredTags(acc) {
		out = append(out, app.apiFeaturedTag(acc, f))
	}
	apiJSON(w, 200, out)
}

func (app *App) apiMyFeaturedTags(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	app.writeFeaturedTags(w, c.Me)
}

func (app *App) apiAccountFeaturedTags(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	acc, ok := app.apiAccountByID(w, r)
	if !ok {
		return
	}
	app.writeFeaturedTags(w, acc)
}

func (app *App) apiFeatureTag(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	f, err := app.FeatureTag(c.Me, readParams(r).Get("name"))
	if err != nil {
		apiError(w, 422, "Validation failed: "+err.Error())
		return
	}
	apiJSON(w, 200, app.apiFeaturedTag(c.Me, *f))
}

func (app *App) apiUnfeatureTagByID(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if err := app.UnfeatureTag(c.Me, r.PathValue("id")); err != nil {
		apiError(w, 404, "Record not found")
		return
	}
	apiJSON(w, 200, obj{})
}

func (app *App) apiFeaturedTagSuggestions(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	for _, t := range app.tagSuggestions(c.Me) {
		out = append(out, app.apiTagFor(c.Me, t))
	}
	apiJSON(w, 200, out)
}

// apiTagFeature handles POST /api/v1/tags/:name/feature and /unfeature (Mastodon 4.4).
func (app *App) apiTagFeature(feature bool) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, c *apiCtx) {
		t := normalizeTag(r.PathValue("name"))
		if t == "" {
			apiError(w, 404, "Record not found")
			return
		}
		var err error
		if feature {
			_, err = app.FeatureTag(c.Me, t)
		} else if err = app.UnfeatureTag(c.Me, t); err == errNotFound {
			err = nil
		}
		if err != nil {
			apiError(w, 422, "Validation failed: "+err.Error())
			return
		}
		apiJSON(w, 200, app.apiTagFor(c.Me, t))
	}
}

// apiTagFor is a Tag entity with the viewer's following/featuring state.
func (app *App) apiTagFor(me *Account, t string) obj {
	m := app.apiTag(t, me != nil && app.followsTag(me, t))
	if me != nil {
		m["featuring"] = app.featuresTag(me, t)
	}
	return m
}

// featuredTagsCollection is the actor's featuredTags collection (Mastodon's shape).
func (app *App) handleFeaturedTagsCollection(w http.ResponseWriter, r *http.Request) {
	acc, err := app.LocalAccount(r.PathValue("name"))
	if err != nil || acc.Suspended {
		http.NotFound(w, r)
		return
	}
	items := []any{}
	for _, f := range app.featuredTags(acc) {
		items = append(items, obj{"type": "Hashtag", "href": app.cfg.BaseURL() + "/tags/" + url.PathEscape(f.Name), "name": "#" + f.Name})
	}
	writeAP(w, obj{"@context": ap.Context, "id": acc.URI + "/collections/tags", "type": "Collection", "totalItems": len(items), "items": items})
}

func (app *App) handleTagFeature(w http.ResponseWriter, r *http.Request, p *Page) {
	tag := normalizeTag(r.PathValue("tag"))
	var err error
	if strings.HasSuffix(r.URL.Path, "/unfeature") {
		err = app.UnfeatureTag(p.Me, tag)
	} else {
		_, err = app.FeatureTag(p.Me, tag)
	}
	if err != nil && err != errNotFound {
		setFlash(w, err.Error())
	}
	app.back(w, r, "/tags/"+url.PathEscape(tag))
}
