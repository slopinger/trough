package server

import (
	"cmp"
	"net/http"
	"strings"
)

func nextURL(r *http.Request, sts []*Status) string {
	if len(sts) < pageSize {
		return ""
	}
	q := r.URL.Query()
	q.Set("max_id", sts[len(sts)-1].ID)
	return r.URL.Path + "?" + q.Encode()
}

func (app *App) handleRoot(w http.ResponseWriter, r *http.Request, p *Page) {
	if p.Me != nil {
		app.handleHome(w, r, p)
		return
	}
	sts, err := app.localTimeline(nil, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title = app.instanceTitle()
	p.Section = "local"
	p.Statuses = sts
	p.NextURL = nextURL(r, sts)
	p.Description = app.getSetting("description")
	p.Registrations = app.registrationMode()
	p.Stats = map[string]int{
		"users": app.countInt(`SELECT COUNT(*) FROM users`),
		"posts": app.countInt(`SELECT COUNT(*) FROM statuses WHERE local=1 AND boost_of_id=''`),
	}
	app.render(w, "landing", p)
}

func (app *App) handleHome(w http.ResponseWriter, r *http.Request, p *Page) {
	sts, err := app.homeTimeline(p.Me, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Section, p.Heading, p.Timeline = "Home", "home", "Home", "home"
	p.Statuses = app.applyFilters(p.Me, sts, "home")
	p.NextURL = nextURL(r, sts)
	p.Compose = &Compose{Visibility: "public"}
	p.Empty = "Your home timeline is empty. Follow some people! Use search to find accounts on other servers, e.g. @someone@mastodon.social."
	app.render(w, "timeline", p)
}

func (app *App) handleLocal(w http.ResponseWriter, r *http.Request, p *Page) {
	sts, err := app.localTimeline(p.Me, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Section, p.Heading = "Local timeline", "local", "Local timeline"
	if p.Me != nil {
		p.Timeline = "local"
	}
	p.Statuses = app.applyFilters(p.Me, sts, "public")
	p.NextURL = nextURL(r, sts)
	p.Empty = "Nobody here has posted anything public yet."
	app.render(w, "timeline", p)
}

func (app *App) handleFederated(w http.ResponseWriter, r *http.Request, p *Page) {
	sts, err := app.federatedTimeline(p.Me, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Section, p.Heading, p.Timeline = "Federated timeline", "federated", "Federated timeline", "federated"
	p.Statuses = app.applyFilters(p.Me, sts, "public")
	p.NextURL = nextURL(r, sts)
	p.Empty = "No public posts known yet."
	app.render(w, "timeline", p)
}

func (app *App) handleTag(w http.ResponseWriter, r *http.Request, p *Page) {
	tag := strings.ToLower(strings.TrimPrefix(r.PathValue("tag"), "#"))
	if t, ok := strings.CutSuffix(tag, ".rss"); ok {
		app.handleTagRSS(w, r, t)
		return
	}
	if wantsAP(r) {
		http.NotFound(w, r)
		return
	}
	sts, err := app.tagTimeline(p.Me, tag, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Heading = "#"+tag, "#"+tag
	p.Tag, p.TagFollowed = tag, app.followsTag(p.Me, tag)
	p.TagStats = app.tagStats(tag)
	p.TagFeatured = p.Me != nil && app.featuresTag(p.Me, tag)
	p.Statuses = app.applyFilters(p.Me, sts, "public")
	p.NextURL = nextURL(r, sts)
	p.Empty = "No public posts with this hashtag are known here."
	if p.Me != nil {
		p.Compose = &Compose{Visibility: "public", Text: "\n\n#" + tag}
	}
	app.render(w, "timeline", p)
}

// handleLink lists the public posts sharing a link (/links?url=), like Mastodon's
// link timeline. Explore's trending links lead here.
func (app *App) handleLink(w http.ResponseWriter, r *http.Request, p *Page) {
	link := strings.TrimSpace(r.URL.Query().Get("url"))
	if !strings.HasPrefix(link, "https://") && !strings.HasPrefix(link, "http://") {
		http.Redirect(w, r, "/explore?tab=links", http.StatusFound)
		return
	}
	sts, err := app.linkTimeline(p.Me, link, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	c := &LinkCard{URL: link}
	app.db.QueryRow(`SELECT title, description, image, site FROM link_cards WHERE url=?`, link).Scan(&c.Title, &c.Description, &c.Image, &c.Site)
	p.LinkCard, p.Section, p.Heading = c, "explore", "Posts about this link"
	p.Title = "Posts about " + cmp.Or(c.Title, link)
	p.Statuses = app.applyFilters(p.Me, sts, "public")
	p.NextURL = nextURL(r, sts)
	p.Empty = "No public posts with this link are known here."
	if p.Me != nil {
		p.Compose = &Compose{Visibility: "public", Text: "\n\n" + link}
	}
	app.render(w, "timeline", p)
}

func (app *App) handleFavourites(w http.ResponseWriter, r *http.Request, p *Page) {
	sts, err := app.favouritesTimeline(p.Me, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Section, p.Heading = "Favourites", "favourites", "Favourites"
	p.Statuses = sts
	p.NextURL = nextURL(r, sts)
	p.Empty = "Nothing favourited yet. Posts you favourite with the star show up here."
	app.render(w, "timeline", p)
}

func (app *App) handleBookmarks(w http.ResponseWriter, r *http.Request, p *Page) {
	sts, err := app.bookmarksTimeline(p.Me, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Section, p.Heading = "Bookmarks", "bookmarks", "Bookmarks"
	p.Statuses = sts
	p.NextURL = nextURL(r, sts)
	p.Empty = "Nothing bookmarked yet. Use the bookmark button on a post to keep it here (only you can see your bookmarks)."
	app.render(w, "timeline", p)
}

// handleNewCount tells the timeline page how many newer posts there are.
func (app *App) handleNewCount(w http.ResponseWriter, r *http.Request, p *Page) {
	since := r.URL.Query().Get("since")
	n := 0
	if since != "" {
		var sts []*Status
		var err error
		switch r.URL.Query().Get("tl") {
		case "local":
			sts, err = app.localTimeline(p.Me, "")
		case "federated":
			sts, err = app.federatedTimeline(p.Me, "")
		default:
			sts, err = app.homeTimeline(p.Me, "")
		}
		if err == nil {
			for _, s := range app.applyFilters(p.Me, sts, "home") {
				if s.ID > since && s.AccountID != p.Me.ID {
					n++
				}
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, "application/json", obj{"new": n, "notifications": app.unreadNotifications(p.Me.ID), "requests": app.followRequestCount(p.Me.ID), "dms": app.unreadConversations(p.Me)})
}
