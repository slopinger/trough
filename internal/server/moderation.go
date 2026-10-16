package server

import (
	"errors"
	"time"
)

// What an admin can do about a report. Each action resolves it.
var reportActions = map[string]string{
	"none":    "Resolved without action",
	"delete":  "Deleted the reported posts",
	"suspend": "Suspended the account",
	"block":   "Blocked the server",
}

// ResolveReport takes action on a report and records who did what. Suspending
// and blocking only apply to remote accounts; local ones can lose posts.
func (app *App) ResolveReport(admin *Account, id, action, note string) (string, error) {
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
	switch action {
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
		if t.IsLocal() {
			return "", errors.New("only accounts on other servers can be suspended here")
		}
		app.SuspendAccount(t)
		msg = "Suspended " + t.Acct() + " and resolved the report."
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
	return msg, err
}

// SuspendAccount hides a remote account: its posts disappear, its activities
// are ignored and follows in both directions end (like a one-account domain block).
func (app *App) SuspendAccount(a *Account) {
	app.db.Exec(`UPDATE accounts SET suspended=1 WHERE id=? AND domain != ''`, a.ID)
	app.db.Exec(`DELETE FROM follows WHERE account_id=? OR target_id=?`, a.ID, a.ID)
	app.db.Exec(`DELETE FROM notifications WHERE from_account_id=?`, a.ID)
}

// UnsuspendAccount lifts a suspension, unless the account's server is blocked.
func (app *App) UnsuspendAccount(a *Account) error {
	if a.IsLocal() {
		return errors.New("not a remote account")
	}
	if app.domainBlocked(a.Domain) {
		return errors.New(a.Domain + " is blocked; unblock the server instead")
	}
	_, err := app.db.Exec(`UPDATE accounts SET suspended=0 WHERE id=?`, a.ID)
	return err
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
