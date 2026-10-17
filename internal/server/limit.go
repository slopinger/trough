package server

import (
	"errors"
	"net/http"
)

// Limiting an account (Mastodon's "limit", formerly "silence"): a softer
// step than suspending. A limited account's posts stay visible to the
// people who follow it, but leave public timelines, hashtags, trends,
// search, the directory and suggestions for everyone else; its
// notifications to people who don't follow it go to the filtered requests
// (the policy's "limited accounts", filtered by default); and its profile
// shows a notice. Nothing is sent to other servers, and refetching the
// actor doesn't lift it.

// limitedSQL hides limited accounts' posts from viewers who don't follow them.
func limitedSQL(v *Account) (string, []any) {
	if v == nil {
		return `NOT EXISTS(SELECT 1 FROM accounts a WHERE a.id=s.account_id AND (a.limited=1 OR ` + domainLimitedSQL + `))`, nil
	}
	return `(s.account_id=? OR NOT EXISTS(SELECT 1 FROM accounts a WHERE a.id=s.account_id AND (a.limited=1 OR ` + domainLimitedSQL + `))
		OR EXISTS(SELECT 1 FROM follows lf WHERE lf.account_id=? AND lf.target_id=s.account_id AND lf.accepted=1))`, []any{v.ID, v.ID}
}

func (app *App) setLimited(a *Account, on bool) error {
	switch {
	case on && a.Limited:
		return errors.New(a.Acct() + " is already limited")
	case !on && !a.Limited:
		return errors.New(a.Acct() + " isn't limited")
	case on && a.IsLocal() && app.isAdmin(a.ID):
		return errors.New("admins can't be limited")
	}
	_, err := app.db.Exec(`UPDATE accounts SET limited=? WHERE id=?`, on, a.ID)
	a.Limited = on
	return err
}

func (app *App) limitedAccounts() []*Account {
	accs, _ := app.accountsQuery(`SELECT ` + accountCols + ` FROM accounts a WHERE a.limited=1 AND a.suspended=0 ORDER BY a.domain, a.username`)
	return accs
}

// POST /admin/accounts/{id}/unlimit
func (app *App) handleUnlimit(w http.ResponseWriter, r *http.Request, p *Page) {
	a, err := app.AccountByID(r.PathValue("id"))
	if err == nil {
		err = app.setLimited(a, false)
	}
	if err != nil {
		setFlash(w, "Couldn't lift the limit: "+err.Error())
	} else {
		setFlash(w, "Lifted the limit on "+a.Acct()+".")
	}
	back := "/admin#limited"
	if r.FormValue("back") == "reports" {
		back = "/admin/reports?tab=resolved"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
