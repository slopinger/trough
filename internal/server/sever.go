package server

import (
	"encoding/csv"
	"net/http"
	"strconv"
	"time"

	"github.com/slopinger/trough/internal/ulid"
)

// Severed relationships (Mastodon 4.3): when the admins block a server or
// suspend a remote account, the follows between it and local accounts end.
// Each local account that lost some gets a severed_relationships
// notification and can download the lost follows and followers on
// /severed_relationships as CSV (the follows in the import format, so they
// can be restored if the block is lifted).

type Severance struct {
	ID, Type, TargetName string // Type: domain_block or account_suspension
	CreatedAt            time.Time
	Followers, Following int // for the account looking at it
}

// severFollows records the follows between local accounts and the remote
// accounts matching where (on accounts a) before they're deleted, and
// notifies the local side.
func (app *App) severFollows(kind, target, where string, args ...any) {
	rows, err := app.db.Query(`SELECT 'following', f.account_id, a.username, a.domain, f.show_reblogs, f.notify
		FROM follows f JOIN accounts a ON a.id=f.target_id JOIN accounts l ON l.id=f.account_id
		WHERE f.accepted=1 AND l.domain='' AND (`+where+`)
		UNION ALL SELECT 'followers', f.target_id, a.username, a.domain, 1, 0
		FROM follows f JOIN accounts a ON a.id=f.account_id JOIN accounts l ON l.id=f.target_id
		WHERE f.accepted=1 AND l.domain='' AND (`+where+`)`, append(append([]any{}, args...), args...)...)
	if err != nil {
		return
	}
	type row struct {
		dir, local, acct string
		reblogs, notify  bool
	}
	var lost []row
	for rows.Next() {
		var r row
		var user, domain string
		if rows.Scan(&r.dir, &r.local, &user, &domain, &r.reblogs, &r.notify) == nil {
			r.acct = user + "@" + domain
			lost = append(lost, r)
		}
	}
	rows.Close()
	if len(lost) == 0 {
		return
	}
	id, now := ulid.New(), time.Now().Unix()
	if _, err := app.db.Exec(`INSERT INTO severances (id,type,target_name,created_at) VALUES (?,?,?,?)`, id, kind, target, now); err != nil {
		return
	}
	notified := map[string]bool{}
	for _, r := range lost {
		app.db.Exec(`INSERT INTO severed_follows (severance_id,account_id,direction,acct,show_reblogs,notify) VALUES (?,?,?,?,?,?)`,
			id, r.local, r.dir, r.acct, r.reblogs, r.notify)
		notified[r.local] = true
	}
	inst := app.instanceActor()
	if inst == nil {
		return
	}
	for local := range notified {
		nid := ulid.New()
		if _, err := app.db.Exec(`INSERT INTO notifications (id,account_id,type,from_account_id,created_at,event_id) VALUES (?,?,?,?,?,?)`,
			nid, local, "severed_relationships", inst.ID, now, id); err == nil {
			app.publishNotification(local, nid)
			app.pushNotification(local, nid)
		}
	}
}

// severances returns the events in which v lost follows (all of them, or the given ids).
func (app *App) severances(v *Account, ids ...string) []*Severance {
	q := `SELECT s.id, s.type, s.target_name, s.created_at,
		SUM(f.direction='followers'), SUM(f.direction='following')
		FROM severances s JOIN severed_follows f ON f.severance_id=s.id WHERE f.account_id=?`
	args := []any{v.ID}
	if len(ids) > 0 {
		in, inArgs := inClause(` AND s.id IN (%s)`, uniq(ids))
		q += in
		args = append(args, inArgs...)
	}
	rows, err := app.db.Query(q+` GROUP BY s.id ORDER BY s.id DESC LIMIT 100`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*Severance
	for rows.Next() {
		s := &Severance{}
		var created int64
		if rows.Scan(&s.ID, &s.Type, &s.TargetName, &created, &s.Followers, &s.Following) == nil {
			s.CreatedAt = time.Unix(created, 0)
			out = append(out, s)
		}
	}
	return out
}

func (s *Severance) Summary() string {
	what := "blocked " + s.TargetName
	if s.Type == "account_suspension" {
		what = "suspended @" + s.TargetName
	}
	return "The moderators " + what
}

func (s *Severance) Lost() string {
	switch {
	case s.Followers > 0 && s.Following > 0:
		return "You lost " + plural(s.Followers, "follower") + " and " + plural(s.Following, "account") + " you followed"
	case s.Followers > 0:
		return "You lost " + plural(s.Followers, "follower")
	}
	return "You no longer follow " + plural(s.Following, "account")
}

func (app *App) apiSeverance(s *Severance) obj {
	return obj{"id": s.ID, "type": s.Type, "purged": false, "target_name": s.TargetName,
		"followers_count": s.Followers, "following_count": s.Following, "created_at": apiTime(s.CreatedAt)}
}

// GET /severed_relationships
func (app *App) handleSeverances(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section = "Severed relationships", "settings"
	p.Severances = app.severances(p.Me)
	app.render(w, "severed", p)
}

// GET /severed_relationships/{id}/{following|followers}.csv
func (app *App) handleSeveranceCSV(w http.ResponseWriter, r *http.Request, p *Page) {
	id, file := r.PathValue("id"), r.PathValue("file")
	dir := map[string]string{"following.csv": "following", "followers.csv": "followers"}[file]
	if dir == "" || len(app.severances(p.Me, id)) == 0 {
		app.notFound(w, p)
		return
	}
	rows, err := app.db.Query(`SELECT acct, show_reblogs, notify FROM severed_follows WHERE severance_id=? AND account_id=? AND direction=? ORDER BY acct`,
		id, p.Me.ID, dir)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+dir+"-"+id+`.csv"`)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if dir == "following" {
		cw.Write([]string{"Account address", "Show boosts", "Notify on new posts", "Languages"})
	} else {
		cw.Write([]string{"Account address"})
	}
	for rows.Next() {
		var acct string
		var reblogs, notify bool
		if rows.Scan(&acct, &reblogs, &notify) != nil {
			continue
		}
		if dir == "following" {
			cw.Write([]string{acct, strconv.FormatBool(reblogs), strconv.FormatBool(notify), ""})
		} else {
			cw.Write([]string{acct})
		}
	}
}
