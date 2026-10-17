package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/slopinger/trough/internal/ap"
)

// "Follow from your server" for visitors: they give their own handle, we
// look up their server's subscribe template (the OStatus rel in WebFinger,
// which Mastodon, GoToSocial, Akkoma and others publish) and send them
// there to follow the account or interact with the post. Servers without
// the template get Mastodon's /authorize_interaction?uri=.

const subscribeRel = "http://ostatus.org/schema/1.0/subscribe"

// remoteInteractURL is where the visitor with handle can act on uri.
func (app *App) remoteInteractURL(ctx context.Context, handle, uri string) (string, error) {
	user, domain, ok := parseHandle(handle)
	if !ok || domain == "" {
		return "", errInvalidHandle
	}
	if _, valid := normalizeDomain(domain); !valid && !app.cfg.Insecure {
		return "", errInvalidHandle
	}
	q := url.Values{"resource": {"acct:" + user + "@" + domain}}
	m, _, err := app.fetchJSON(ctx, app.cfg.Scheme+"://"+domain+"/.well-known/webfinger?"+q.Encode(), "application/jrd+json, application/json")
	if err == nil {
		for _, l := range ap.Objects(m["links"]) {
			if ap.Str(l, "rel") == subscribeRel {
				if t := ap.Str(l, "template"); strings.Contains(t, "{uri}") {
					target := strings.Replace(t, "{uri}", url.QueryEscape(uri), 1)
					if u, err := url.Parse(target); err == nil && (u.Scheme == "https" || app.cfg.Insecure && u.Scheme == "http") {
						return target, nil
					}
				}
			}
		}
	}
	return app.cfg.Scheme + "://" + domain + "/authorize_interaction?uri=" + url.QueryEscape(uri), nil
}

var errInvalidHandle = errors.New("enter your full address, like you@mastodon.social")

// handleInteractPage asks a visitor for their own address (GET /interact?uri=...).
func (app *App) handleInteractPage(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title = "Interact from your server"
	p.Query = r.URL.Query().Get("uri")
	p.NextURL = r.URL.Query().Get("back")
	if !strings.HasPrefix(p.Query, app.cfg.BaseURL()+"/") {
		app.notFound(w, p)
		return
	}
	app.render(w, "interact", p)
}

// handleRemoteInteract handles the "Follow from your server" / "Interact
// from your server" form (target: a local account or post IRI).
func (app *App) handleRemoteInteract(w http.ResponseWriter, r *http.Request, p *Page) {
	target := r.FormValue("uri")
	back := r.FormValue("back")
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
		back = "/"
	}
	if !strings.HasPrefix(target, app.cfg.BaseURL()+"/") {
		http.Error(w, "not one of ours", http.StatusBadRequest)
		return
	}
	if app.interacts.blocked(clientIP(r)) {
		setFlash(w, "Too many attempts; try again in a few minutes.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	app.interacts.hit(clientIP(r))
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	dest, err := app.remoteInteractURL(ctx, r.FormValue("handle"), target)
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
