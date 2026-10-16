package server

import (
	"database/sql"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (app *App) apiUploadMedia(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if err := r.ParseMultipartForm(8 << 20); err != nil || r.MultipartForm == nil || len(r.MultipartForm.File["file"]) == 0 {
		apiError(w, 422, "Validation failed: file is missing")
		return
	}
	fh, err := r.MultipartForm.File["file"][0].Open()
	if err != nil {
		apiError(w, 422, "could not read file")
		return
	}
	data, _ := io.ReadAll(io.LimitReader(fh, maxMediaSize+1))
	fh.Close()
	at, err := app.saveUpload(c.Me, data, r.FormValue("description"))
	if err != nil {
		apiError(w, 422, "Validation failed: "+err.Error())
		return
	}
	apiJSON(w, 200, app.apiMedia(at))
}

func (app *App) apiMediaByID(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	at := &Attachment{}
	err := app.db.QueryRow(`SELECT id, account_id, url, media_type, description, file, width, height FROM attachments WHERE id=? AND account_id=?`,
		r.PathValue("id"), c.Me.ID).Scan(&at.ID, &at.AccountID, &at.URL, &at.MediaType, &at.Description, &at.File, &at.Width, &at.Height)
	if err != nil {
		apiError(w, 404, "Record not found")
		return
	}
	if r.Method == http.MethodDelete {
		// only uploads that aren't part of a post (Mastodon 4.4)
		var statusID sql.NullString
		app.db.QueryRow(`SELECT status_id FROM attachments WHERE id=?`, at.ID).Scan(&statusID)
		if statusID.Valid {
			apiError(w, 422, "Media attachment is currently used by a status")
			return
		}
		if at.File != "" {
			os.Remove(filepath.Join(app.mediaDir, filepath.Base(at.File)))
		}
		app.db.Exec(`DELETE FROM attachments WHERE id=?`, at.ID)
		apiJSON(w, 200, obj{})
		return
	}
	if r.Method == http.MethodPut {
		if d := readParams(r).Get("description"); true {
			at.Description = truncateRunes(strings.TrimSpace(d), 1500)
			app.db.Exec(`UPDATE attachments SET description=? WHERE id=?`, at.Description, at.ID)
		}
	}
	apiJSON(w, 200, app.apiMedia(at))
}
