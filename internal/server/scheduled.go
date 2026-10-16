package server

// Scheduled posts: stored with their parameters and published by a
// background loop when they're due.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"git.freifick.net/slop/trough/internal/ulid"
)

const minScheduleAhead = 5 * time.Minute

type ScheduledStatus struct {
	ID          string
	Input       PostInput
	ScheduledAt time.Time
	Media       []*Attachment
}

func (app *App) Schedule(me *Account, in PostInput, at time.Time) (*ScheduledStatus, error) {
	if time.Until(at) < minScheduleAhead {
		return nil, errors.New("scheduled posts must be at least 5 minutes in the future")
	}
	if strings.TrimSpace(in.Text) == "" && len(in.MediaIDs) == 0 && in.Poll == nil {
		return nil, errors.New("post is empty")
	}
	if app.countInt(`SELECT COUNT(*) FROM scheduled_statuses WHERE account_id=?`, me.ID) >= 300 {
		return nil, errors.New("too many scheduled posts")
	}
	params, _ := json.Marshal(in)
	media, _ := json.Marshal(in.MediaIDs)
	if in.MediaIDs == nil {
		media = []byte("[]")
	}
	s := &ScheduledStatus{ID: ulid.New(), Input: in, ScheduledAt: at.UTC().Truncate(time.Second)}
	_, err := app.db.Exec(`INSERT INTO scheduled_statuses (id, account_id, params, media_ids, scheduled_at, created_at) VALUES (?,?,?,?,?,?)`,
		s.ID, me.ID, string(params), string(media), s.ScheduledAt.Unix(), time.Now().Unix())
	return s, err
}

func (app *App) scheduledFor(me *Account, id string) ([]*ScheduledStatus, error) {
	q := `SELECT id, params, scheduled_at FROM scheduled_statuses WHERE account_id=?`
	args := []any{me.ID}
	if id != "" {
		q += ` AND id=?`
		args = append(args, id)
	}
	rows, err := app.db.Query(q+` ORDER BY scheduled_at`, args...)
	if err != nil {
		return nil, err
	}
	var out []*ScheduledStatus
	for rows.Next() {
		s := &ScheduledStatus{}
		var params string
		var at int64
		rows.Scan(&s.ID, &params, &at)
		json.Unmarshal([]byte(params), &s.Input)
		s.ScheduledAt = time.Unix(at, 0)
		out = append(out, s)
	}
	rows.Close()
	for _, s := range out {
		if len(s.Input.MediaIDs) > 0 {
			q, args := inClause(`SELECT id, status_id, account_id, url, media_type, description, file, width, height, preview_url, preview_file, focus_x, focus_y FROM attachments WHERE id IN (%s)`, s.Input.MediaIDs)
			if r2, err := app.db.Query(q, args...); err == nil {
				for r2.Next() {
					at := &Attachment{}
					var sid *string
					r2.Scan(&at.ID, &sid, &at.AccountID, &at.URL, &at.MediaType, &at.Description, &at.File, &at.Width, &at.Height, &at.PreviewURL, &at.PreviewFile, &at.FocusX, &at.FocusY)
					s.Media = append(s.Media, at)
				}
				r2.Close()
			}
		}
	}
	return out, nil
}

// publishDue posts scheduled statuses whose time has come.
func (app *App) publishDue(ctx context.Context) {
	rows, err := app.db.Query(`SELECT id, account_id, params FROM scheduled_statuses WHERE scheduled_at <= ? ORDER BY scheduled_at LIMIT 20`, time.Now().Unix())
	if err != nil {
		return
	}
	type due struct{ id, account, params string }
	var list []due
	for rows.Next() {
		var d due
		rows.Scan(&d.id, &d.account, &d.params)
		list = append(list, d)
	}
	rows.Close()
	for _, d := range list {
		// claim it first so a slow post can't be published twice
		if res, err := app.db.Exec(`DELETE FROM scheduled_statuses WHERE id=?`, d.id); err != nil {
			continue
		} else if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		me, err := app.AccountByID(d.account)
		if err != nil {
			continue
		}
		var in PostInput
		json.Unmarshal([]byte(d.params), &in)
		if in.Poll != nil {
			// the poll runs for the duration chosen when scheduling
			if dur := in.Poll.EndTime.Sub(in.PollStart); dur > 0 && !in.PollStart.IsZero() {
				in.Poll.EndTime = time.Now().Add(dur).UTC().Truncate(time.Second)
			}
		}
		if _, err := app.PostStatus(ctx, me, in); err != nil {
			log.Printf("scheduled post %s of @%s failed: %v", d.id, me.Username, err)
		}
	}
}

func (app *App) scheduleLoop(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			app.publishDue(ctx)
		}
	}
}

// ---- web ----

func (app *App) handleScheduled(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section = "Scheduled posts", "scheduled"
	p.Scheduled, _ = app.scheduledFor(p.Me, "")
	app.render(w, "scheduled", p)
}

func (app *App) handleCancelScheduled(w http.ResponseWriter, r *http.Request, p *Page) {
	app.db.Exec(`DELETE FROM scheduled_statuses WHERE id=? AND account_id=?`, r.PathValue("id"), p.Me.ID)
	setFlash(w, "Scheduled post cancelled.")
	http.Redirect(w, r, "/scheduled", http.StatusSeeOther)
}

// parseScheduleTime reads the compose form's time: an RFC 3339 value (set
// by JS from the local time) or a bare datetime-local value taken as UTC.
func parseScheduleTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ---- API ----

func (app *App) apiScheduled(s *ScheduledStatus) obj {
	media := []any{}
	for _, m := range s.Media {
		media = append(media, app.apiMedia(m))
	}
	var poll any
	if s.Input.Poll != nil {
		var opts []string
		for _, o := range s.Input.Poll.Options {
			opts = append(opts, o.Name)
		}
		poll = obj{"options": opts, "multiple": s.Input.Poll.Multiple, "expires_in": int(s.Input.Poll.EndTime.Sub(s.Input.PollStart).Seconds())}
	}
	ids := s.Input.MediaIDs
	if ids == nil {
		ids = []string{}
	}
	return obj{"id": s.ID, "scheduled_at": apiTime(s.ScheduledAt), "media_attachments": media, "params": obj{
		"text": s.Input.Text, "spoiler_text": s.Input.Spoiler, "visibility": s.Input.Visibility, "sensitive": s.Input.Sensitive,
		"in_reply_to_id": nullIfEmpty(s.Input.InReplyTo), "media_ids": ids, "poll": poll, "scheduled_at": nil,
		"idempotency": nil, "application_id": nil, "with_rate_limit": false}}
}

func (app *App) apiListScheduled(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	list, _ := app.scheduledFor(c.Me, "")
	out := []any{}
	for _, s := range list {
		out = append(out, app.apiScheduled(s))
	}
	apiJSON(w, 200, out)
}

func (app *App) apiScheduled1(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	list, _ := app.scheduledFor(c.Me, r.PathValue("id"))
	if len(list) == 0 {
		apiError(w, 404, "Record not found")
		return
	}
	s := list[0]
	switch r.Method {
	case http.MethodDelete:
		app.db.Exec(`DELETE FROM scheduled_statuses WHERE id=?`, s.ID)
		apiJSON(w, 200, obj{})
		return
	case http.MethodPut:
		at, err := time.Parse(time.RFC3339, readParams(r).Get("scheduled_at"))
		if err != nil || time.Until(at) < minScheduleAhead {
			apiError(w, 422, "Validation failed: scheduled_at must be at least 5 minutes in the future")
			return
		}
		s.ScheduledAt = at.UTC().Truncate(time.Second)
		app.db.Exec(`UPDATE scheduled_statuses SET scheduled_at=? WHERE id=?`, s.ScheduledAt.Unix(), s.ID)
	}
	apiJSON(w, 200, app.apiScheduled(s))
}
