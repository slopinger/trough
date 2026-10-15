package server

// Custom emoji: admins upload images, people use them as :shortcode: in
// posts, names and bios, and they federate as Emoji tags.

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var shortcodeRe = regexp.MustCompile(`^[A-Za-z0-9_]{2,32}$`)
var shortcodeUseRe = regexp.MustCompile(`:([A-Za-z0-9_]{2,32}):`)

type CustomEmoji struct {
	Shortcode, File, URL, MediaType string
}

func (app *App) customEmojis() []CustomEmoji {
	rows, err := app.db.Query(`SELECT shortcode, file, url, media_type FROM custom_emojis ORDER BY shortcode`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []CustomEmoji
	for rows.Next() {
		var e CustomEmoji
		rows.Scan(&e.Shortcode, &e.File, &e.URL, &e.MediaType)
		out = append(out, e)
	}
	return out
}

// emojisIn returns the local custom emoji used in the given texts.
func (app *App) emojisIn(texts ...string) []Emoji {
	seen := map[string]bool{}
	var codes []string
	for _, t := range texts {
		for _, m := range shortcodeUseRe.FindAllStringSubmatch(t, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				codes = append(codes, m[1])
			}
		}
	}
	if len(codes) == 0 {
		return nil
	}
	q, args := inClause(`SELECT shortcode, url FROM custom_emojis WHERE shortcode IN (%s)`, codes)
	rows, err := app.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Emoji
	for rows.Next() {
		var e Emoji
		rows.Scan(&e.Shortcode, &e.URL)
		out = append(out, e)
	}
	return out
}

// emojiTags are the ActivityPub Emoji tags for emoji used by a local object.
func (app *App) emojiTags(list []Emoji) []any {
	var out []any
	for _, e := range list {
		if !strings.HasPrefix(e.URL, app.cfg.BaseURL()+"/") {
			continue
		}
		out = append(out, app.emojiObject(e.Shortcode, e.URL))
	}
	return out
}

func (app *App) emojiObject(code, u string) obj {
	icon := imageObj(u)
	return obj{"id": app.cfg.BaseURL() + "/emojis/" + code, "type": "Emoji", "name": ":" + code + ":", "updated": "2026-01-01T00:00:00Z", "icon": icon}
}

func (app *App) AddCustomEmoji(code string, data []byte) error {
	code = strings.Trim(strings.TrimSpace(code), ":")
	if !shortcodeRe.MatchString(code) {
		return errors.New("shortcodes are 2-32 characters of letters, digits and _")
	}
	if len(data) > 512<<10 {
		return errors.New("emoji images are at most 512 KB")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width > 1024 || cfg.Height > 1024 || cfg.Width == 0 {
		return errors.New("use a PNG, GIF or WebP image of at most 1024×1024 pixels")
	}
	ext, mt := ".png", "image/png"
	switch format {
	case "gif":
		ext, mt = ".gif", "image/gif" // keep animations
	case "png":
	case "webp":
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return errors.New("could not decode the image")
		}
		var b bytes.Buffer
		png.Encode(&b, img)
		data = b.Bytes()
	default:
		return errors.New("use a PNG, GIF or WebP image")
	}
	name := newID() + ext
	if err := os.WriteFile(filepath.Join(app.mediaDir, name), data, 0o640); err != nil {
		return err
	}
	var old string
	app.db.QueryRow(`SELECT file FROM custom_emojis WHERE shortcode=?`, code).Scan(&old)
	if _, err := app.db.Exec(`INSERT INTO custom_emojis (shortcode, file, url, media_type, created_at) VALUES (?,?,?,?,?)
		ON CONFLICT(shortcode) DO UPDATE SET file=excluded.file, url=excluded.url, media_type=excluded.media_type`,
		code, name, app.cfg.BaseURL()+"/media/"+name, mt, time.Now().Unix()); err != nil {
		return err
	}
	if old != "" {
		os.Remove(filepath.Join(app.mediaDir, filepath.Base(old)))
	}
	return nil
}

func (app *App) DeleteCustomEmoji(code string) {
	var file string
	if app.db.QueryRow(`SELECT file FROM custom_emojis WHERE shortcode=?`, code).Scan(&file) == nil {
		os.Remove(filepath.Join(app.mediaDir, filepath.Base(file)))
		app.db.Exec(`DELETE FROM custom_emojis WHERE shortcode=?`, code)
	}
}

func (app *App) handleEmojiObject(w http.ResponseWriter, r *http.Request) {
	var u string
	if app.db.QueryRow(`SELECT url FROM custom_emojis WHERE shortcode=?`, r.PathValue("code")).Scan(&u) != nil {
		http.NotFound(w, r)
		return
	}
	writeAP(w, withContext(app.emojiObject(r.PathValue("code"), u)))
}

func (app *App) handleAdminEmoji(w http.ResponseWriter, r *http.Request, p *Page) {
	if code := r.FormValue("delete"); code != "" {
		app.DeleteCustomEmoji(code)
		setFlash(w, "Emoji :"+code+": deleted.")
		http.Redirect(w, r, "/admin#emoji", http.StatusSeeOther)
		return
	}
	data, ok := app.readImageField(r, "image")
	if !ok {
		setFlash(w, "Choose an image.")
	} else if err := app.AddCustomEmoji(r.FormValue("shortcode"), data); err != nil {
		setFlash(w, err.Error())
	} else {
		setFlash(w, "Emoji added.")
	}
	http.Redirect(w, r, "/admin#emoji", http.StatusSeeOther)
}

func (app *App) apiCustomEmojis(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	for _, e := range app.customEmojis() {
		out = append(out, obj{"shortcode": e.Shortcode, "url": e.URL, "static_url": e.URL, "visible_in_picker": true, "category": nil})
	}
	apiJSON(w, 200, out)
}
