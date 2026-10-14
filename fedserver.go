package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Federation-facing HTTP endpoints: WebFinger, NodeInfo, actors, objects
// and collections.

func wantsAP(r *http.Request) bool {
	a := r.Header.Get("Accept")
	return strings.Contains(a, "application/activity+json") || strings.Contains(a, "application/ld+json")
}

func writeJSON(w http.ResponseWriter, contentType string, v any) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Vary", "Accept")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeAP(w http.ResponseWriter, v any) {
	writeJSON(w, `application/activity+json; charset=utf-8`, v)
}

func (app *App) handleWebfinger(w http.ResponseWriter, r *http.Request) {
	res := r.URL.Query().Get("resource")
	if res == "" {
		http.Error(w, "missing resource", http.StatusBadRequest)
		return
	}
	var acc *Account
	if strings.HasPrefix(res, "http://") || strings.HasPrefix(res, "https://") {
		a, err := app.accountWhere(`a.domain='' AND (a.uri=? OR a.url=?)`, res, res)
		if err == nil {
			acc = a
		}
	} else if user, domain, ok := parseHandle(res); ok && strings.EqualFold(domain, app.cfg.Host) {
		if inst := app.instanceActor(); strings.EqualFold(user, inst.Username) {
			acc = inst
		} else if a, err := app.LocalAccount(user); err == nil {
			acc = a
		}
	}
	if acc == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, "application/jrd+json; charset=utf-8", obj{
		"subject": "acct:" + acc.Username + "@" + app.cfg.Host,
		"aliases": []string{acc.URI, acc.URL},
		"links": []obj{
			{"rel": "self", "type": apMimeType, "href": acc.URI},
			{"rel": "http://webfinger.net/rel/profile-page", "type": "text/html", "href": acc.URL},
			{"rel": "http://ostatus.org/schema/1.0/subscribe", "template": app.cfg.BaseURL() + "/authorize_interaction?uri={uri}"},
		},
	})
}

func (app *App) handleHostMeta(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xrd+xml; charset=utf-8")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<XRD xmlns="http://docs.oasis-open.org/ns/xri/xrd-1.0">
  <Link rel="lrdd" template="%s/.well-known/webfinger?resource={uri}"/>
</XRD>
`, app.cfg.BaseURL())
}

func (app *App) handleNodeInfoIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, "application/json", obj{"links": []obj{
		{"rel": "http://nodeinfo.diaspora.software/ns/schema/2.0", "href": app.cfg.BaseURL() + "/nodeinfo/2.0"},
		{"rel": "http://nodeinfo.diaspora.software/ns/schema/2.1", "href": app.cfg.BaseURL() + "/nodeinfo/2.1"},
	}})
}

func (app *App) handleNodeInfo(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	if version != "2.0" && version != "2.1" {
		http.NotFound(w, r)
		return
	}
	users := app.countInt(`SELECT COUNT(*) FROM users`)
	posts := app.countInt(`SELECT COUNT(*) FROM statuses WHERE local=1 AND boost_of_id=''`)
	month := app.countInt(`SELECT COUNT(*) FROM users WHERE last_seen > strftime('%s','now') - 30*86400`)
	half := app.countInt(`SELECT COUNT(*) FROM users WHERE last_seen > strftime('%s','now') - 182*86400`)
	sw := obj{"name": softwareName, "version": softwareVersion}
	if version == "2.1" {
		sw["repository"] = "https://git.freifick.net/slop/trough"
		sw["homepage"] = "https://git.freifick.net/slop/trough"
	}
	writeJSON(w, `application/json; profile="http://nodeinfo.diaspora.software/ns/schema/`+version+`#"`, obj{
		"version":           version,
		"software":          sw,
		"protocols":         []string{"activitypub"},
		"services":          obj{"inbound": []string{}, "outbound": []string{}},
		"openRegistrations": false,
		"usage": obj{
			"users":      obj{"total": users, "activeMonth": month, "activeHalfyear": half},
			"localPosts": posts,
		},
		"metadata": obj{
			"nodeName":        app.instanceTitle(),
			"nodeDescription": app.getSetting("description"),
		},
	})
}

// handleInstanceAPI is a tiny subset of Mastodon's /api/v1/instance, which
// many crawlers and tools look at.
func (app *App) handleInstanceAPI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, "application/json", obj{
		"uri":               app.cfg.Host,
		"title":             app.instanceTitle(),
		"short_description": app.getSetting("description"),
		"description":       app.getSetting("description"),
		"version":           "4.0.0 (compatible; " + softwareName + " " + softwareVersion + ")",
		"registrations":     false,
		"approval_required": true,
		"stats": obj{
			"user_count":   app.countInt(`SELECT COUNT(*) FROM users`),
			"status_count": app.countInt(`SELECT COUNT(*) FROM statuses WHERE local=1 AND boost_of_id=''`),
			"domain_count": app.countInt(`SELECT COUNT(DISTINCT domain) FROM accounts WHERE domain != ''`),
		},
		"languages": []string{"en"},
	})
}

func (app *App) handleActor(w http.ResponseWriter, r *http.Request) {
	acc, err := app.LocalAccount(r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !wantsAP(r) {
		http.Redirect(w, r, "/@"+acc.Username, http.StatusFound)
		return
	}
	writeAP(w, app.actorJSON(acc))
}

func (app *App) handleInstanceActorDoc(w http.ResponseWriter, r *http.Request) {
	if !wantsAP(r) {
		http.Redirect(w, r, "/about", http.StatusFound)
		return
	}
	writeAP(w, app.actorJSON(app.instanceActor()))
}

// apStatus serves the ActivityPub form of a local status, enforcing
// visibility via the signature of the requester for non-public posts.
func (app *App) apStatus(w http.ResponseWriter, r *http.Request, username, id string, activity bool) {
	st, err := app.StatusByID(id)
	if err != nil || !st.Local {
		http.NotFound(w, r)
		return
	}
	sts, err := app.hydrate(nil, []*Status{st})
	if err != nil || len(sts) == 0 || !strings.EqualFold(sts[0].Account.Username, username) {
		http.NotFound(w, r)
		return
	}
	st = sts[0]
	if st.Visibility != "public" && st.Visibility != "unlisted" {
		signer, err := app.verifyRequest(r, nil)
		if err != nil || !app.mayFetch(signer, st) {
			http.NotFound(w, r)
			return
		}
	}
	if st.IsBoost() {
		if st.BoostOf == nil {
			http.NotFound(w, r)
			return
		}
		writeAP(w, app.announceActivity(st))
		return
	}
	if activity {
		writeAP(w, app.createActivity(st))
		return
	}
	writeAP(w, withContext(app.noteJSON(st)))
}

// mayFetch reports whether a remote actor is an intended recipient of st.
func (app *App) mayFetch(signer *Account, st *Status) bool {
	for _, m := range st.Mentions {
		if m.ID == signer.ID {
			return true
		}
	}
	if st.Visibility == "private" {
		var x int
		return app.db.QueryRow(`SELECT 1 FROM follows WHERE account_id=? AND target_id=? AND accepted=1`, signer.ID, st.AccountID).Scan(&x) == nil
	}
	return false
}

func (app *App) handleAPStatus(w http.ResponseWriter, r *http.Request) {
	if !wantsAP(r) {
		http.Redirect(w, r, "/@"+r.PathValue("name")+"/"+r.PathValue("id"), http.StatusFound)
		return
	}
	app.apStatus(w, r, r.PathValue("name"), r.PathValue("id"), false)
}

func (app *App) handleAPStatusActivity(w http.ResponseWriter, r *http.Request) {
	app.apStatus(w, r, r.PathValue("name"), r.PathValue("id"), true)
}

func (app *App) handleAPReplies(w http.ResponseWriter, r *http.Request) {
	st, err := app.StatusByID(r.PathValue("id"))
	if err != nil || !st.Local || st.Visibility == "private" || st.Visibility == "direct" {
		http.NotFound(w, r)
		return
	}
	rows, err := app.db.Query(`SELECT uri FROM statuses WHERE in_reply_to_id=? AND visibility IN ('public','unlisted') ORDER BY id LIMIT 100`, st.ID)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	items := []string{}
	for rows.Next() {
		var u string
		rows.Scan(&u)
		items = append(items, u)
	}
	rows.Close()
	id := st.URI + "/replies"
	writeAP(w, withContext(obj{
		"id": id, "type": "Collection", "totalItems": len(items),
		"first": obj{"id": id + "?page=true", "type": "CollectionPage", "partOf": id, "items": items},
	}))
}

func (app *App) handleOutbox(w http.ResponseWriter, r *http.Request) {
	acc, err := app.LocalAccount(r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	id := acc.Outbox
	total := app.countInt(`SELECT COUNT(*) FROM statuses WHERE account_id=? AND visibility IN ('public','unlisted')`, acc.ID)
	if r.URL.Query().Get("page") == "" {
		writeAP(w, withContext(obj{
			"id": id, "type": "OrderedCollection", "totalItems": total,
			"first": id + "?page=true", "last": id + "?page=true&min_id=0",
		}))
		return
	}
	maxID := r.URL.Query().Get("max_id")
	sts, err := app.queryStatuses(nil, `s.account_id=? AND s.visibility IN ('public','unlisted')`, []any{acc.ID}, maxID, pageSize, false)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	items := []any{}
	for _, st := range sts {
		if st.IsBoost() {
			items = append(items, delContext(app.announceActivity(st)))
		} else {
			items = append(items, delContext(app.createActivity(st)))
		}
	}
	page := obj{
		"id": id + "?page=true", "type": "OrderedCollectionPage", "partOf": id, "orderedItems": items,
	}
	if maxID != "" {
		page["id"] = id + "?page=true&max_id=" + url.QueryEscape(maxID)
	}
	if len(sts) == pageSize {
		page["next"] = id + "?page=true&max_id=" + sts[len(sts)-1].ID
	}
	writeAP(w, withContext(page))
}

func delContext(m obj) obj {
	delete(m, "@context")
	return m
}

func (app *App) handleFollowCollection(followers bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acc, err := app.LocalAccount(r.PathValue("name"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		id, q := acc.FollowingURI, `SELECT COUNT(*) FROM follows WHERE account_id=? AND accepted=1`
		if followers {
			id, q = acc.FollowersURI, `SELECT COUNT(*) FROM follows WHERE target_id=? AND accepted=1`
		}
		total := app.countInt(q, acc.ID)
		pageStr := r.URL.Query().Get("page")
		if pageStr == "" {
			writeAP(w, withContext(obj{"id": id, "type": "OrderedCollection", "totalItems": total, "first": id + "?page=1"}))
			return
		}
		page, _ := strconv.Atoi(pageStr)
		if page < 1 {
			page = 1
		}
		list, err := app.followList(acc, followers, false, (page-1)*40)
		if err != nil {
			http.Error(w, "db error", 500)
			return
		}
		items := []string{}
		for _, a := range list {
			items = append(items, a.URI)
		}
		p := obj{"id": fmt.Sprintf("%s?page=%d", id, page), "type": "OrderedCollectionPage", "partOf": id,
			"totalItems": total, "orderedItems": items}
		if len(list) == 40 {
			p["next"] = fmt.Sprintf("%s?page=%d", id, page+1)
		}
		writeAP(w, withContext(p))
	}
}

func (app *App) handleEmptyCollection(w http.ResponseWriter, r *http.Request) {
	writeAP(w, withContext(obj{
		"id": app.cfg.BaseURL() + r.URL.Path, "type": "OrderedCollection", "totalItems": 0, "orderedItems": []any{},
	}))
}
