package server

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// Embedding posts on other websites, like Mastodon: /s/:id/embed is a small
// page meant for an iframe, /s/:id/embed_code shows the code to paste, and
// /api/oembed describes it to sites that discover it via oEmbed. Only local
// public and unlisted posts can be embedded.

const embedWidth = 400

// embeddable loads a local post anyone may see and embed.
func (app *App) embeddable(id string) (*Status, bool) {
	st, err := app.loadStatus(nil, id)
	if err != nil || !st.Local || st.IsBoost() || (st.Visibility != "public" && st.Visibility != "unlisted") || st.Account.Suspended {
		return nil, false
	}
	return st, true
}

func (app *App) embedURL(st *Status) string { return app.cfg.BaseURL() + st.Path() + "/embed" }

// embedHTML is the code other sites paste: the iframe plus a script that
// fits its height to the post.
func (app *App) embedHTML(st *Status) string {
	return fmt.Sprintf(`<iframe src="%s" class="trough-embed" style="max-width: 100%%; border: 0" width="%d" height="300" allowfullscreen="allowfullscreen" title="Post by %s"></iframe><script src="%s/embed.js" async="async"></script>`,
		html.EscapeString(app.embedURL(st)), embedWidth, html.EscapeString(st.Account.Acct()), app.cfg.BaseURL())
}

func (app *App) handleEmbed(w http.ResponseWriter, r *http.Request, p *Page) {
	st, ok := app.embeddable(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	// other sites may frame this page, and only this one
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; media-src 'self'; style-src 'self'; script-src 'self'; "+
		"object-src 'none'; frame-ancestors *; form-action 'none'; base-uri 'none'")
	w.Header().Del("X-Frame-Options")
	p.Me, p.Status, p.Title = nil, st, st.Account.Name()
	app.render(w, "embed", p)
}

func (app *App) handleEmbedCode(w http.ResponseWriter, r *http.Request, p *Page) {
	st, ok := app.embeddable(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	p.Status, p.Title = st, "Embed a post"
	p.EmbedCode = app.embedHTML(st)
	app.render(w, "embedcode", p)
}

// GET /api/oembed?url=…
func (app *App) apiOEmbed(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := app.statusForURL(r.URL.Query().Get("url"))
	if !ok {
		apiError(w, 404, "Record not found")
		return
	}
	apiJSON(w, 200, obj{"type": "rich", "version": "1.0", "title": "Post by " + st.Account.Name(),
		"author_name": st.Account.Name(), "author_url": st.Account.URL, "provider_name": app.instanceTitle(),
		"provider_url": app.cfg.BaseURL() + "/", "cache_age": 86400, "html": app.embedHTML(st), "width": embedWidth, "height": nil})
}

// statusForURL finds an embeddable post by any of its local URLs.
func (app *App) statusForURL(raw string) (*Status, bool) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Host, app.cfg.Host) {
		return nil, false
	}
	st, ok := app.embeddable(path.Base(u.Path))
	if !ok {
		return nil, false
	}
	u.RawQuery, u.Fragment = "", ""
	for _, known := range []string{st.URI, st.URL, app.cfg.BaseURL() + st.Path()} {
		if u.String() == known {
			return st, true
		}
	}
	return nil, false
}

// oembedLink is the discovery link for a post's page, "" if it can't be embedded.
func (app *App) oembedLink(st *Status) string {
	if _, ok := app.embeddable(st.ID); !ok {
		return ""
	}
	return app.cfg.BaseURL() + "/api/oembed?" + url.Values{"url": {st.URL}}.Encode()
}

// handleEmbedScript serves /embed.js for the websites posts are embedded on.
func (app *App) handleEmbedScript(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/embed.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(b)
}
