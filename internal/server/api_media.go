package server

import (
	"database/sql"
	"io"
	"net/http"
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
	if f := r.FormValue("focus"); f != "" {
		app.setFocus(at, f)
	}
	if thumb := formFile(r, "thumbnail"); thumb != nil && at.Kind() != "image" {
		if err := app.setPoster(at, thumb); err != nil {
			apiError(w, 422, "Validation failed: thumbnail: "+err.Error())
			return
		}
	}
	apiJSON(w, 200, app.apiMedia(at))
}

func (app *App) apiMediaByID(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	at := &Attachment{}
	err := app.db.QueryRow(`SELECT id, account_id, url, media_type, description, file, width, height, preview_url, preview_file, focus_x, focus_y FROM attachments WHERE id=? AND account_id=?`,
		r.PathValue("id"), c.Me.ID).Scan(&at.ID, &at.AccountID, &at.URL, &at.MediaType, &at.Description, &at.File, &at.Width, &at.Height, &at.PreviewURL, &at.PreviewFile, &at.FocusX, &at.FocusY)
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
		app.removeMediaFiles(`id=?`, at.ID)
		app.db.Exec(`DELETE FROM attachments WHERE id=?`, at.ID)
		apiJSON(w, 200, obj{})
		return
	}
	if r.Method == http.MethodPut {
		p := readParams(r)
		if p.Has("description") {
			at.Description = truncateRunes(strings.TrimSpace(p.Get("description")), 1500)
			app.db.Exec(`UPDATE attachments SET description=? WHERE id=?`, at.Description, at.ID)
		}
		if p.Has("focus") {
			app.setFocus(at, p.Get("focus"))
		}
		if thumb := formFile(r, "thumbnail"); thumb != nil {
			if err := app.setPoster(at, thumb); err != nil {
				apiError(w, 422, "Validation failed: thumbnail: "+err.Error())
				return
			}
		}
	}
	apiJSON(w, 200, app.apiMedia(at))
}

// formFile reads an uploaded image field of a parsed multipart form (nil if absent).
func formFile(r *http.Request, name string) []byte {
	if r.MultipartForm == nil || len(r.MultipartForm.File[name]) == 0 {
		return nil
	}
	f, err := r.MultipartForm.File[name][0].Open()
	if err != nil {
		return nil
	}
	defer f.Close()
	data, _ := io.ReadAll(io.LimitReader(f, maxUploadSize+1))
	return data
}
