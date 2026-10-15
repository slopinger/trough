package server

import (
	"archive/zip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Downloading your archive (Settings → Import and export): a zip laid out
// like Mastodon's, with actor.json, outbox.json (all your posts and boosts,
// including private ones and direct messages), likes.json, bookmarks.json,
// your avatar and header, and the media you uploaded.

const archiveInterval = time.Hour

var (
	archiveMu   sync.Mutex
	archiveLast = map[string]time.Time{} // by account ID
)

func (app *App) writeArchive(me *Account, w io.Writer) error {
	zw := zip.NewWriter(w)
	defer zw.Close()
	writeJSON := func(name string, v any) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	addFile := func(name, path string) {
		src, err := os.Open(path)
		if err != nil {
			return
		}
		defer src.Close()
		if dst, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store}); err == nil {
			io.Copy(dst, src)
		}
	}
	local := func(u string) string { // a file in our media directory, or ""
		if !strings.Contains(u, "/media/") {
			return ""
		}
		return filepath.Join(app.mediaDir, filepath.Base(u))
	}

	actor := app.actorJSON(me)
	for _, k := range []string{"icon", "image"} {
		if m, ok := actor[k].(obj); ok {
			if p := local(me.Avatar); k == "icon" && p != "" {
				addFile("avatar"+filepath.Ext(p), p)
				m["url"] = "avatar" + filepath.Ext(p)
			}
			if p := local(me.Header); k == "image" && p != "" {
				addFile("header"+filepath.Ext(p), p)
				m["url"] = "header" + filepath.Ext(p)
			}
		}
	}
	if err := writeJSON("actor.json", actor); err != nil {
		return err
	}

	var items []any
	maxID := ""
	for {
		sts, err := app.queryStatuses(me, `s.account_id=?`, []any{me.ID}, maxID, 200, false)
		if err != nil {
			return err
		}
		for _, st := range sts {
			if st.IsBoost() {
				items = append(items, delContext(app.announceActivity(st)))
				continue
			}
			act := delContext(app.createActivity(st))
			for _, at := range st.Attachments {
				if p := local(at.URL); p != "" && at.File != "" {
					addFile("media_attachments/files/"+filepath.Base(p), p)
				}
			}
			items = append(items, act)
		}
		if len(sts) < 200 {
			break
		}
		maxID = sts[len(sts)-1].ID
	}
	if err := writeJSON("outbox.json", withContext(obj{"id": "outbox.json", "type": "OrderedCollection",
		"totalItems": len(items), "orderedItems": items})); err != nil {
		return err
	}
	uris := func(q string) []any {
		out := []any{}
		rows, err := app.db.Query(q, me.ID)
		if err != nil {
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var u string
			rows.Scan(&u)
			out = append(out, u)
		}
		return out
	}
	likes := uris(`SELECT s.uri FROM likes l JOIN statuses s ON s.id=l.status_id WHERE l.account_id=? ORDER BY l.created_at`)
	if err := writeJSON("likes.json", withContext(obj{"id": "likes.json", "type": "OrderedCollection", "totalItems": len(likes), "orderedItems": likes})); err != nil {
		return err
	}
	marks := uris(`SELECT s.uri FROM bookmarks b JOIN statuses s ON s.id=b.status_id WHERE b.account_id=? ORDER BY b.created_at`)
	return writeJSON("bookmarks.json", withContext(obj{"id": "bookmarks.json", "type": "OrderedCollection", "totalItems": len(marks), "orderedItems": marks}))
}

func (app *App) handleArchive(w http.ResponseWriter, r *http.Request, p *Page) {
	archiveMu.Lock()
	if last, ok := archiveLast[p.Me.ID]; ok && time.Since(last) < archiveInterval {
		archiveMu.Unlock()
		setFlash(w, "You can download your archive once an hour.")
		http.Redirect(w, r, "/settings#import", http.StatusSeeOther)
		return
	}
	archiveLast[p.Me.ID] = time.Now()
	archiveMu.Unlock()
	name := "trough-" + p.Me.Username + "-" + time.Now().UTC().Format("20060102") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	app.writeArchive(p.Me, w)
}
