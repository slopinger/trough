package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/slopinger/trough/internal/ulid"
)

// Moderation warnings ("strikes", Mastodon 4.3): when the admins act on a
// local account from a report (warn, delete posts, limit, suspend), the
// account gets a moderation_warning notification from the server's own
// account, linking to /disputes/strikes/{id}. It shows the moderators'
// message and the posts concerned (kept as excerpts, since deleting them is
// final), and the account can appeal once within 20 days. Admins decide
// appeals on /admin/reports?tab=appeals; approving one lifts a limit or
// suspension that the warning put on. Warnings never leave this server.

const appealWindow = 20 * 24 * time.Hour

type Warning struct {
	ID, Action, Text, ReportID string
	Account                    *Account
	Statuses                   []WarnedStatus
	CreatedAt                  time.Time
	AppealText, AppealState    string // AppealState: "" (none), pending, approved, rejected
	AppealAt, Overruled        time.Time
	AppealBy                   *Account // the admin who decided the appeal
}

// WarnedStatus is an excerpt of a post the warning is about.
type WarnedStatus struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	At   int64  `json:"at"`
}

func (s WarnedStatus) Time() time.Time { return time.Unix(s.At, 0) }

// warningActions maps report actions to Mastodon's strike actions.
var warningActions = map[string]string{"warn": "none", "delete": "delete_statuses", "limit": "silence", "suspend": "suspend", "sensitive": "sensitive"}

func (w *Warning) Summary() string {
	switch w.Action {
	case "delete_statuses":
		return "Some of your posts were removed"
	case "silence":
		return "Your account has been limited"
	case "suspend":
		return "Your account has been suspended"
	case "sensitive":
		return "Your media is marked as sensitive"
	}
	return "Your account received a moderation warning"
}

// Label names the decision in the admins' list of an account's warnings.
func (w *Warning) Label() string {
	switch w.Action {
	case "delete_statuses":
		return "Posts removed"
	case "silence":
		return "Limited"
	case "suspend":
		return "Suspended"
	case "sensitive":
		return "Media marked sensitive"
	}
	return "Warning"
}

// AdminSummary is the decision as the admins see it on an appeal.
func (w *Warning) AdminSummary() string {
	switch w.Action {
	case "delete_statuses":
		return "the removal of their posts"
	case "silence":
		return "the limit on their account"
	case "suspend":
		return "their suspension"
	case "sensitive":
		return "marking their media as sensitive"
	}
	return "a warning"
}

func (w *Warning) Explanation() string {
	switch w.Action {
	case "delete_statuses":
		return "The moderators removed the posts below. They were deleted here and on other servers."
	case "silence":
		return "Your posts are still visible to your followers, but no longer show up in public timelines, hashtags or search for anyone else, and your notifications to people who don't follow you are filtered."
	case "suspend":
		return "Your account can't be used while it is suspended. Nothing has been deleted."
	case "sensitive":
		return "Pictures, videos and audio in your posts, including earlier ones, are hidden behind a click from now on."
	}
	return "This is a warning; nothing about your account has changed."
}

func (w *Warning) CanAppeal() bool {
	return w.AppealState == "" && w.Overruled.IsZero() && time.Since(w.CreatedAt) < appealWindow
}

func (w *Warning) AppealUntil() time.Time { return w.CreatedAt.Add(appealWindow) }

// warnAccount records a warning for a local account and notifies it.
func (app *App) warnAccount(a *Account, action, text, reportID string, sts []*Status) (*Warning, error) {
	if !a.IsLocal() || !app.isUser(a.ID) {
		return nil, errors.New("only accounts on this server can be warned")
	}
	excerpts := []WarnedStatus{}
	for _, s := range sts {
		t := plainText(s.Content)
		if s.Spoiler != "" {
			t = s.Spoiler + " — " + t
		}
		if t == "" {
			t = "(media)"
		}
		excerpts = append(excerpts, WarnedStatus{ID: s.ID, Text: truncateRunes(t, 500), At: s.CreatedAt.Unix()})
	}
	js, _ := json.Marshal(excerpts)
	w := &Warning{ID: ulid.New(), Action: action, Text: truncateRunes(strings.TrimSpace(text), 2000), ReportID: reportID,
		Account: a, Statuses: excerpts, CreatedAt: time.Now()}
	if _, err := app.db.Exec(`INSERT INTO account_warnings (id,account_id,action,text,report_id,statuses,created_at) VALUES (?,?,?,?,?,?,?)`,
		w.ID, a.ID, w.Action, w.Text, reportID, string(js), w.CreatedAt.Unix()); err != nil {
		return nil, err
	}
	// straight in: the notification policy and mutes don't apply to the moderators
	if inst := app.instanceActor(); inst != nil {
		nid := ulid.New()
		if _, err := app.db.Exec(`INSERT INTO notifications (id,account_id,type,from_account_id,created_at,warning_id) VALUES (?,?,?,?,?,?)`,
			nid, a.ID, "moderation_warning", inst.ID, w.CreatedAt.Unix(), w.ID); err == nil {
			app.publishNotification(a.ID, nid)
			app.pushNotification(a.ID, nid)
		}
	}
	return w, nil
}

const warningCols = `w.id, w.account_id, w.action, w.text, w.report_id, w.statuses, w.created_at, w.appeal_text, w.appeal_at, w.appeal_state, w.appeal_by, w.overruled_at`

func (app *App) loadWarnings(where string, args ...any) []*Warning {
	rows, err := app.db.Query(`SELECT `+warningCols+` FROM account_warnings w WHERE `+where+` ORDER BY w.id DESC LIMIT 100`, args...)
	if err != nil {
		return nil
	}
	var out []*Warning
	var accIDs []string
	type ids struct{ acc, by string }
	var raw []ids
	for rows.Next() {
		w := &Warning{}
		var acc, js, by string
		var created, appealAt, overruled int64
		if rows.Scan(&w.ID, &acc, &w.Action, &w.Text, &w.ReportID, &js, &created, &w.AppealText, &appealAt, &w.AppealState, &by, &overruled) != nil {
			continue
		}
		json.Unmarshal([]byte(js), &w.Statuses)
		w.CreatedAt = time.Unix(created, 0)
		if appealAt > 0 {
			w.AppealAt = time.Unix(appealAt, 0)
		}
		if overruled > 0 {
			w.Overruled = time.Unix(overruled, 0)
		}
		out = append(out, w)
		raw = append(raw, ids{acc, by})
		accIDs = append(accIDs, acc)
		if by != "" {
			accIDs = append(accIDs, by)
		}
	}
	rows.Close()
	accs, _ := app.accountsByIDs(accIDs)
	kept := out[:0]
	for i, w := range out {
		if w.Account = accs[raw[i].acc]; w.Account != nil {
			w.AppealBy = accs[raw[i].by]
			kept = append(kept, w)
		}
	}
	return kept
}

func (app *App) warningsByID(ids []string) map[string]*Warning {
	out := map[string]*Warning{}
	if len(ids) == 0 {
		return out
	}
	q, args := inClause(`w.id IN (%s)`, uniq(ids))
	for _, w := range app.loadWarnings(q, args...) {
		out[w.ID] = w
	}
	return out
}

func (app *App) ownWarning(me *Account, id string) *Warning {
	if ws := app.loadWarnings(`w.id=? AND w.account_id=?`, id, me.ID); len(ws) == 1 {
		return ws[0]
	}
	return nil
}

func (app *App) appeal(me *Account, id, text string) error {
	w := app.ownWarning(me, id)
	text = strings.TrimSpace(text)
	switch {
	case w == nil:
		return errors.New("that warning doesn't exist")
	case !w.CanAppeal():
		return errors.New("this can't be appealed anymore")
	case text == "":
		return errors.New("say why the moderators should reconsider")
	case utf8.RuneCountInString(text) > 2000:
		return errors.New("the appeal is too long (2000 characters at most)")
	}
	_, err := app.db.Exec(`UPDATE account_warnings SET appeal_text=?, appeal_at=?, appeal_state='pending' WHERE id=? AND appeal_state=''`,
		text, time.Now().Unix(), id)
	return err
}

// decideAppeal approves or rejects a pending appeal. Approving overrules the
// warning and lifts the limit or suspension it came with.
func (app *App) decideAppeal(admin *Account, id string, approve bool) (*Warning, error) {
	ws := app.loadWarnings(`w.id=? AND w.appeal_state='pending'`, id)
	if len(ws) == 0 {
		return nil, errors.New("there's no open appeal like that")
	}
	w := ws[0]
	if w.Account.ID == admin.ID {
		return nil, errors.New("someone else has to decide your own appeal")
	}
	now := time.Now().Unix()
	if !approve {
		_, err := app.db.Exec(`UPDATE account_warnings SET appeal_state='rejected', appeal_by=? WHERE id=?`, admin.ID, id)
		return w, err
	}
	switch {
	case w.Action == "silence" && w.Account.Limited:
		if err := app.setLimited(w.Account, false); err != nil {
			return nil, err
		}
	case w.Action == "suspend" && w.Account.Suspended:
		if err := app.UnsuspendAccount(w.Account); err != nil {
			return nil, err
		}
	case w.Action == "sensitive" && w.Account.ForceSensitive:
		if err := app.setForceSensitive(w.Account, false); err != nil {
			return nil, err
		}
	}
	_, err := app.db.Exec(`UPDATE account_warnings SET appeal_state='approved', appeal_by=?, overruled_at=? WHERE id=?`, admin.ID, now, id)
	return w, err
}

func (app *App) apiWarning(w *Warning) obj {
	ids := []string{}
	for _, s := range w.Statuses {
		ids = append(ids, s.ID)
	}
	var appeal any
	if w.AppealState != "" {
		appeal = obj{"text": w.AppealText, "state": w.AppealState}
	}
	return obj{"id": w.ID, "action": w.Action, "text": w.Text, "status_ids": ids, "target_account": app.apiAccount(w.Account),
		"appeal": appeal, "created_at": apiTime(w.CreatedAt)}
}

// GET /disputes/strikes and /disputes/strikes/{id}
func (app *App) handleStrikes(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section = "Account status", "settings"
	if id := r.PathValue("id"); id != "" {
		if p.Warning = app.ownWarning(p.Me, id); p.Warning == nil {
			app.notFound(w, p)
			return
		}
		p.Title = p.Warning.Summary()
	} else {
		p.Warnings = app.loadWarnings(`w.account_id=?`, p.Me.ID)
	}
	app.render(w, "strikes", p)
}

// POST /disputes/strikes/{id}/appeal
func (app *App) handleAppeal(w http.ResponseWriter, r *http.Request, p *Page) {
	id := r.PathValue("id")
	if err := app.appeal(p.Me, id, r.FormValue("text")); err != nil {
		setFlash(w, "Couldn't send the appeal: "+err.Error()+".")
	} else {
		setFlash(w, "Appeal sent. The moderators will look at it.")
	}
	http.Redirect(w, r, "/disputes/strikes/"+id, http.StatusSeeOther)
}

// POST /admin/appeals/{id}/{action}
func (app *App) handleAppealDecision(w http.ResponseWriter, r *http.Request, p *Page) {
	var err error
	var wa *Warning
	switch r.PathValue("action") {
	case "approve":
		if wa, err = app.decideAppeal(p.Me, r.PathValue("id"), true); err == nil {
			setFlash(w, "Approved the appeal of "+wa.Account.Acct()+".")
		}
	case "reject":
		if wa, err = app.decideAppeal(p.Me, r.PathValue("id"), false); err == nil {
			setFlash(w, "Rejected the appeal of "+wa.Account.Acct()+".")
		}
	default:
		err = errors.New("unknown action")
	}
	if err != nil {
		setFlash(w, "Couldn't do that: "+err.Error())
	}
	http.Redirect(w, r, "/admin/reports?tab=appeals", http.StatusSeeOther)
}
