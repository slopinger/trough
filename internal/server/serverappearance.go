package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Server appearance, like Mastodon's admin settings: a thumbnail (banner
// image) shown on the landing and about pages and given to apps through the
// instance API, and custom CSS loaded after trough's own stylesheet. Both
// live in the settings table; the thumbnail file is in media/.

const maxCustomCSS = 20000

// thumbnailURL is the server's banner image, or the logo without one.
func (app *App) thumbnailURL() string {
	if u := app.getSetting("thumbnail"); u != "" {
		return u
	}
	return app.cfg.BaseURL() + "/static/favicon.svg"
}

// POST /admin/appearance
func (app *App) handleAdminAppearance(w http.ResponseWriter, r *http.Request, p *Page) {
	msg := "Appearance saved."
	if r.FormValue("remove_thumbnail") != "" {
		if old := app.getSetting("thumbnail"); old != "" {
			if strings.HasPrefix(old, app.cfg.BaseURL()+"/media/") {
				os.Remove(filepath.Join(app.mediaDir, filepath.Base(old)))
			}
			app.db.Exec(`DELETE FROM settings WHERE key='thumbnail'`)
		}
		msg = "Thumbnail removed."
	} else if data, ok := app.readImageField(r, "thumbnail"); ok {
		if u, err := app.replaceImage(app.instanceActor(), app.getSetting("thumbnail"), data); err != nil {
			msg = "Couldn't use that image: " + err.Error()
		} else {
			app.setSetting("thumbnail", u)
		}
	}
	if r.FormValue("save_css") != "" {
		css := strings.TrimSpace(strings.ReplaceAll(r.FormValue("custom_css"), "\r\n", "\n"))
		if utf8.RuneCountInString(css) > maxCustomCSS {
			msg = "The custom CSS is too long (" + strconv.Itoa(maxCustomCSS) + " characters at most)."
		} else {
			app.setSetting("custom_css", css)
			app.setSetting("custom_css_version", strconv.FormatInt(time.Now().UnixNano(), 36))
		}
	}
	setFlash(w, msg)
	http.Redirect(w, r, "/admin#appearance", http.StatusSeeOther)
}

// GET /custom.css
func (app *App) handleCustomCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400") // the page links it with a version
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write([]byte(app.getSetting("custom_css")))
}
