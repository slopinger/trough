package server

import (
	"errors"
	"net/http"
	"time"
)

// Sign-ups that need approval (Mastodon's "approval required" registration
// mode). Applicants say why they want to join; their account is created
// suspended with users.approved=0, so it can't log in, isn't shown and isn't
// federated until an admin approves it. Rejecting deletes it completely and
// frees the name: nothing about it was ever sent to other servers. Invite
// links skip the approval.

type PendingSignup struct {
	Account *Account
	Reason  string
	Created time.Time
}

func (app *App) signupPending(accountID string) bool {
	var approved bool
	err := app.db.QueryRow(`SELECT approved FROM users WHERE account_id=?`, accountID).Scan(&approved)
	return err == nil && !approved
}

// holdForApproval marks a new account as waiting for an admin.
func (app *App) holdForApproval(acc *Account, reason string) {
	app.db.Exec(`UPDATE users SET approved=0, signup_reason=? WHERE account_id=?`, truncateRunes(reason, 1000), acc.ID)
	app.db.Exec(`UPDATE accounts SET suspended=1, discoverable=0 WHERE id=?`, acc.ID)
}

func (app *App) pendingSignups() []PendingSignup {
	rows, err := app.db.Query(`SELECT account_id, signup_reason, created_at FROM users WHERE approved=0 ORDER BY created_at`)
	if err != nil {
		return nil
	}
	type row struct {
		id, reason string
		created    int64
	}
	var list []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.reason, &r.created) == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	var out []PendingSignup
	for _, r := range list {
		if acc, err := app.AccountByID(r.id); err == nil {
			out = append(out, PendingSignup{acc, r.reason, time.Unix(r.created, 0)})
		}
	}
	return out
}

func (app *App) approveSignup(accountID string) error {
	if !app.signupPending(accountID) {
		return errors.New("that sign-up isn't waiting any more")
	}
	app.db.Exec(`UPDATE users SET approved=1 WHERE account_id=?`, accountID)
	_, err := app.db.Exec(`UPDATE accounts SET suspended=0, discoverable=1 WHERE id=?`, accountID)
	return err
}

func (app *App) rejectSignup(accountID string) error {
	if !app.signupPending(accountID) {
		return errors.New("that sign-up isn't waiting any more")
	}
	if err := app.deleteAccountRows(accountID); err != nil {
		return err
	}
	_, err := app.db.Exec(`DELETE FROM accounts WHERE id=?`, accountID)
	return err
}

// POST /admin/signups/{id}/{approve|reject}
func (app *App) handleSignupDecision(w http.ResponseWriter, r *http.Request, p *Page) {
	id := r.PathValue("id")
	acc, err := app.AccountByID(id)
	if err == nil {
		switch r.PathValue("action") {
		case "approve":
			if err = app.approveSignup(id); err == nil {
				setFlash(w, "@"+acc.Username+" can log in now.")
			}
		case "reject":
			if err = app.rejectSignup(id); err == nil {
				setFlash(w, "Rejected @"+acc.Username+"; the account is gone.")
			}
		default:
			err = errors.New("unknown action")
		}
	}
	if err != nil {
		setFlash(w, err.Error())
	}
	http.Redirect(w, r, "/admin#signups", http.StatusSeeOther)
}
