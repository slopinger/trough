package server

import (
	"errors"
	"time"

	"git.freifick.net/slop/trough/internal/ap"
)

// What an admin can do about a report. Each action resolves it.
var reportActions = map[string]string{
	"none":      "Resolved without action",
	"delete":    "Deleted the reported posts",
	"suspend":   "Suspended the account",
	"limit":     "Limited the account",
	"block":     "Blocked the server",
	"warn":      "Warned the account",
	"sensitive": "Marked the account's media as sensitive",
}

// ResolveReport takes action on a report and records who did what. Suspending
// and blocking only apply to remote accounts; local ones can lose posts.
func (app *App) ResolveReport(admin *Account, id, action, note string) (string, error) {
	return app.resolveReport(admin, id, action, note, "")
}

// resolveReport also gives a local account a moderation warning (see
// warnings.go) for any action but "none", with the admins' message.
func (app *App) resolveReport(admin *Account, id, action, note, message string) (string, error) {
	r := app.loadReports(`r.id=?`, []any{id}, 1)
	if len(r) == 0 {
		return "", errors.New("report not found")
	}
	rep := r[0]
	if !rep.Resolved.IsZero() {
		return "", errors.New("this report is already resolved")
	}
	if _, ok := reportActions[action]; !ok {
		return "", errors.New("unknown action")
	}
	t := rep.Target
	msg := "Report resolved."
	warn := warningActions[action] != "" && t.IsLocal()
	if action == "warn" && (!warn || !app.isUser(t.ID)) {
		return "", errors.New("only accounts on this server can be warned")
	}
	switch action {
	case "warn":
		msg = "Warned " + t.Acct() + " and resolved the report."
	case "sensitive":
		if err := app.setForceSensitive(t, true); err != nil {
			return "", err
		}
		msg = "Marked the media of " + t.Acct() + " as sensitive and resolved the report."
	case "delete":
		n := 0
		for _, st := range rep.Statuses {
			if st.Local {
				if owner, err := app.AccountByID(st.AccountID); err == nil && app.DeleteStatus(owner, st) == nil {
					n++
				}
			} else if app.purgeStatus(st.ID) == nil {
				n++
			}
		}
		if n == 0 {
			return "", errors.New("there are no reported posts left to delete")
		}
		msg = "Deleted " + plural(n, "post") + " and resolved the report."
	case "suspend":
		if err := app.AdminSuspend(admin, t); err != nil {
			return "", err
		}
		msg = "Suspended " + t.Acct() + " and resolved the report."
	case "limit":
		if t.IsLocal() && t.ID == admin.ID {
			return "", errors.New("you can't limit yourself")
		}
		if err := app.setLimited(t, true); err != nil {
			return "", err
		}
		msg = "Limited " + t.Acct() + " and resolved the report."
	case "block":
		if t.IsLocal() {
			return "", errors.New("that account is on this server")
		}
		if err := app.BlockServer(t.Domain, "reported: "+truncateRunes(rep.Comment, 200)); err != nil {
			return "", err
		}
		msg = "Blocked " + t.Domain + " and resolved the report."
	}
	_, err := app.db.Exec(`UPDATE reports SET resolved_at=?, action_taken=?, resolved_by=?, note=? WHERE id=? AND resolved_at=0`,
		time.Now().Unix(), action, admin.ID, truncateRunes(note, 2000), id)
	if err == nil && warn && app.isUser(t.ID) {
		if _, werr := app.warnAccount(t, warningActions[action], message, id, rep.Statuses); werr == nil && action != "warn" {
			msg += " " + t.Acct() + " was notified."
		}
	}
	return msg, err
}

// SuspendAccount hides a remote account: its posts disappear, its activities
// are ignored and follows in both directions end (like a one-account domain block).
// Local accounts go to suspendLocal instead.
func (app *App) SuspendAccount(a *Account) {
	if a.IsLocal() {
		app.suspendLocal(a, true)
		return
	}
	app.severFollows("account_suspension", a.Acct(), `a.id=?`, a.ID)
	app.db.Exec(`UPDATE accounts SET suspended=1, remote_suspended=0 WHERE id=? AND domain != ''`, a.ID)
	app.db.Exec(`DELETE FROM follows WHERE account_id=? OR target_id=?`, a.ID, a.ID)
	app.db.Exec(`DELETE FROM notifications WHERE from_account_id=?`, a.ID)
}

// AdminSuspend is an admin suspending an account (from a report or the user list).
func (app *App) AdminSuspend(admin, a *Account) error {
	switch {
	case a.ID == admin.ID:
		return errors.New("you can't suspend yourself")
	case a.IsLocal() && a.ActorType == "Application":
		return errors.New("that's the server's own account")
	case a.IsLocal() && !app.isUser(a.ID):
		return errors.New("that account was deleted")
	case a.Suspended:
		return errors.New(a.Acct() + " is already suspended")
	}
	app.SuspendAccount(a)
	return nil
}

// UnsuspendAccount lifts a suspension, unless the account's server is blocked.
func (app *App) UnsuspendAccount(a *Account) error {
	if a.IsLocal() {
		if !app.isUser(a.ID) {
			return errors.New("that account was deleted")
		}
		if app.signupPending(a.ID) {
			return errors.New("@" + a.Username + " is waiting for sign-up approval; approve it on /admin")
		}
		app.suspendLocal(a, false)
		return nil
	}
	if app.domainBlocked(a.Domain) {
		return errors.New(a.Domain + " is blocked; unblock the server instead")
	}
	_, err := app.db.Exec(`UPDATE accounts SET suspended=0, remote_suspended=0 WHERE id=?`, a.ID)
	return err
}

// A suspended local account keeps its user row, posts, media and follows, so
// the suspension can be lifted; a deleted one has lost its user row. While
// suspended, nobody can log in or use its tokens, its profile and posts are
// hidden here, and the actor document says "suspended": true with an empty
// profile. Like Mastodon, other servers learn about it through an Update of
// the actor (and again when the suspension is lifted).
func (app *App) suspendLocal(a *Account, on bool) {
	app.db.Exec(`UPDATE accounts SET suspended=? WHERE id=? AND domain=''`, on, a.ID)
	if on {
		app.db.Exec(`DELETE FROM sessions WHERE account_id=?`, a.ID)
	}
	if acc, err := app.AccountByID(a.ID); err == nil {
		*a = *acc
		app.sendActorUpdate(acc)
	}
}

func (app *App) sendActorUpdate(acc *Account) {
	app.enqueue(acc, uniq(append(app.followerInboxes(acc.ID), app.followingInboxes(acc.ID)...)), app.updateActorActivity(acc))
}

func (app *App) isUser(accountID string) bool {
	var n int
	app.db.QueryRow(`SELECT COUNT(*) FROM users WHERE account_id=?`, accountID).Scan(&n)
	return n > 0
}

// followRemoteSuspension mirrors a suspension announced by the account's own
// server ("suspended": true in its actor, as Mastodon sends). Suspensions by
// our admins or domain blocks are never lifted this way.
func (app *App) followRemoteSuspension(a *Account, suspended bool) {
	if suspended && !a.Suspended {
		app.db.Exec(`UPDATE accounts SET suspended=1, remote_suspended=1 WHERE id=?`, a.ID)
		a.Suspended = true
	} else if !suspended && a.Suspended && !app.domainBlocked(a.Domain) {
		if res, err := app.db.Exec(`UPDATE accounts SET suspended=0, remote_suspended=0 WHERE id=? AND remote_suspended=1`, a.ID); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				a.Suspended = false
			}
		}
	}
}

// liftsOwnSuspension lets an account suspended by its own server through to
// announce the end of that suspension with an Update of itself.
func (app *App) liftsOwnSuspension(signer *Account, act obj) bool {
	if ap.Type(act) != "Update" || ap.ID(act["object"]) != signer.URI {
		return false
	}
	var remote bool
	app.db.QueryRow(`SELECT remote_suspended FROM accounts WHERE id=?`, signer.ID).Scan(&remote)
	return remote
}

// loadReports returns reports matching where (on reports r), newest first,
// with the accounts, reported posts and earlier reports about the same account.
func (app *App) loadReports(where string, args []any, limit int) []*Report {
	rows, err := app.db.Query(`SELECT r.id, r.account_id, r.target_id, r.status_ids, r.comment, r.forwarded, r.created_at,
		r.resolved_at, r.action_taken, r.resolved_by, r.note,
		(SELECT COUNT(*) FROM reports o WHERE o.target_id=r.target_id AND o.id != r.id)
		FROM reports r WHERE `+where+` ORDER BY r.created_at DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil
	}
	type raw struct {
		r                          *Report
		from, target, sids, solver string
	}
	var list []raw
	for rows.Next() {
		r := &Report{}
		var x raw
		var created, resolved int64
		rows.Scan(&r.ID, &x.from, &x.target, &x.sids, &r.Comment, &r.Forwarded, &created, &resolved, &r.Action, &x.solver, &r.Note, &r.Earlier)
		r.CreatedAt = time.Unix(created, 0)
		if resolved > 0 {
			r.Resolved = time.Unix(resolved, 0)
		}
		x.r = r
		list = append(list, x)
	}
	rows.Close()
	var out []*Report
	for _, x := range list {
		x.r.Reporter, _ = app.AccountByID(x.from)
		x.r.Target, _ = app.AccountByID(x.target)
		if x.solver != "" {
			x.r.ResolvedBy, _ = app.AccountByID(x.solver)
		}
		var ids []string
		jsonUnmarshal([]byte(x.sids), &ids)
		if len(ids) > 0 {
			// admins see reported posts whatever their visibility
			q, args := inClause(`SELECT `+statusCols+` FROM statuses s WHERE s.id IN (%s)`, ids)
			if sts, err := app.scanStatuses(q, args...); err == nil {
				x.r.Statuses, _ = app.hydrate(nil, sts)
			}
		}
		if x.r.Reporter != nil && x.r.Target != nil {
			x.r.Remote = !x.r.Reporter.IsLocal()
			out = append(out, x.r)
		}
	}
	return out
}

func (app *App) openReports() []*Report { return app.loadReports(`r.resolved_at=0`, nil, 50) }

func (app *App) resolvedReports() []*Report { return app.loadReports(`r.resolved_at>0`, nil, 50) }

// ActionLabel describes what was done about a resolved report.
func (r *Report) ActionLabel() string {
	if l, ok := reportActions[r.Action]; ok {
		return l
	}
	return "Resolved" // before trough recorded actions
}
