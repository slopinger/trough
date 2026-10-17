package server

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/slopinger/trough/internal/ulid"
)

// The admin page of one account (/admin/accounts/{id}), like Mastodon's:
// what's known about it, its reports and warnings, private notes the admins
// leave each other, and the moderation actions without going through a
// report. Actions on local accounts can carry a message and become a
// warning (warnings.go).

type ModNote struct {
	ID, Content string
	Author      *Account
	CreatedAt   time.Time
}

type AdminAccountView struct {
	Account                     *Account
	IsUser, IsAdmin, Pending    bool
	LastSeen                    time.Time
	SignupReason                string
	Posts, Followers, Following int
	Reports                     []*Report
	Warnings                    []*Warning
	Notes                       []*ModNote
}

func (app *App) adminAccountView(a *Account) *AdminAccountView {
	v := &AdminAccountView{Account: a, Reports: app.loadReports(`r.target_id=?`, []any{a.ID}, 50),
		Warnings: app.loadWarnings(`w.account_id=?`, a.ID), Notes: app.moderationNotes(a.ID)}
	var seen int64
	var approved bool
	if app.db.QueryRow(`SELECT admin, last_seen, approved, signup_reason FROM users WHERE account_id=?`, a.ID).Scan(&v.IsAdmin, &seen, &approved, &v.SignupReason) == nil {
		v.IsUser, v.Pending = true, !approved
		if seen > 0 {
			v.LastSeen = time.Unix(seen, 0)
		}
	}
	v.Posts = app.countInt(`SELECT COUNT(*) FROM statuses WHERE account_id=? AND boost_of_id=''`, a.ID)
	v.Followers = app.countInt(`SELECT COUNT(*) FROM follows WHERE target_id=? AND accepted=1`, a.ID)
	v.Following = app.countInt(`SELECT COUNT(*) FROM follows WHERE account_id=? AND accepted=1`, a.ID)
	return v
}

func (app *App) moderationNotes(accountID string) []*ModNote {
	rows, err := app.db.Query(`SELECT id, author_id, content, created_at FROM moderation_notes WHERE account_id=? ORDER BY id`, accountID)
	if err != nil {
		return nil
	}
	var out []*ModNote
	var authors []string
	for rows.Next() {
		n := &ModNote{Author: &Account{}}
		var created int64
		if rows.Scan(&n.ID, &n.Author.ID, &n.Content, &created) == nil {
			n.CreatedAt = time.Unix(created, 0)
			out = append(out, n)
			authors = append(authors, n.Author.ID)
		}
	}
	rows.Close()
	accs, _ := app.accountsByIDs(authors)
	for _, n := range out {
		if a := accs[n.Author.ID]; a != nil {
			n.Author = a
		}
	}
	return out
}

func (app *App) addModerationNote(admin, a *Account, content string) error {
	content = strings.TrimSpace(content)
	switch {
	case content == "":
		return errors.New("the note is empty")
	case utf8.RuneCountInString(content) > 2000:
		return errors.New("the note is too long (2000 characters at most)")
	}
	_, err := app.db.Exec(`INSERT INTO moderation_notes (id,account_id,author_id,content,created_at) VALUES (?,?,?,?,?)`,
		ulid.New(), a.ID, admin.ID, content, time.Now().Unix())
	return err
}

// moderateAccount takes an action on an account directly. message only
// applies to local accounts, as the warning they get.
func (app *App) moderateAccount(admin, a *Account, action, message string) (string, error) {
	local := a.IsLocal() && app.isUser(a.ID)
	if a.IsLocal() && a.ID == admin.ID && action != "unlimit" && action != "unsuspend" && action != "unsensitive" {
		return "", errors.New("you can't do that to yourself")
	}
	msg := ""
	switch action {
	case "warn":
		if !local {
			return "", errors.New("only accounts on this server can be warned")
		}
		msg = "Warned " + a.Acct() + "."
	case "limit":
		if err := app.setLimited(a, true); err != nil {
			return "", err
		}
		msg = "Limited " + a.Acct() + "."
	case "unlimit":
		if err := app.setLimited(a, false); err != nil {
			return "", err
		}
		return "Lifted the limit on " + a.Acct() + ".", nil
	case "suspend":
		if err := app.AdminSuspend(admin, a); err != nil {
			return "", err
		}
		msg = "Suspended " + a.Acct() + "."
	case "sensitive":
		if err := app.setForceSensitive(a, true); err != nil {
			return "", err
		}
		msg = "Marked the media of " + a.Acct() + " as sensitive."
	case "unsensitive":
		if err := app.setForceSensitive(a, false); err != nil {
			return "", err
		}
		return "Media of " + a.Acct() + " is no longer forced sensitive.", nil
	case "unsuspend":
		if err := app.UnsuspendAccount(a); err != nil {
			return "", err
		}
		return "Lifted the suspension of " + a.Acct() + ".", nil
	default:
		return "", errors.New("unknown action")
	}
	if local {
		if _, err := app.warnAccount(a, warningActions[action], message, "", nil); err == nil && action != "warn" {
			msg += " They were notified."
		}
	}
	return msg, nil
}

// GET /admin/accounts/{id}
func (app *App) handleAdminAccount(w http.ResponseWriter, r *http.Request, p *Page) {
	a, err := app.AccountByID(r.PathValue("id"))
	if err != nil {
		app.notFound(w, p)
		return
	}
	p.Title, p.Section = a.Acct(), "admin"
	p.AdminAccount = app.adminAccountView(a)
	app.render(w, "adminaccount", p)
}

// POST /admin/accounts/{id}/action and /admin/accounts/{id}/notes
func (app *App) handleAdminAccountPost(w http.ResponseWriter, r *http.Request, p *Page) {
	a, err := app.AccountByID(r.PathValue("id"))
	if err != nil {
		app.notFound(w, p)
		return
	}
	back := "/admin/accounts/" + a.ID
	switch r.PathValue("what") {
	case "action":
		msg, err := app.moderateAccount(p.Me, a, r.FormValue("action"), r.FormValue("message"))
		if err != nil {
			msg = "Couldn't do that: " + err.Error()
		}
		setFlash(w, msg)
	case "notes":
		if id := r.FormValue("delete"); id != "" {
			app.db.Exec(`DELETE FROM moderation_notes WHERE id=? AND account_id=? AND author_id=?`, id, a.ID, p.Me.ID)
		} else if err := app.addModerationNote(p.Me, a, r.FormValue("content")); err != nil {
			setFlash(w, "Couldn't add the note: "+err.Error()+".")
		}
		back += "#notes"
	default:
		app.notFound(w, p)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
