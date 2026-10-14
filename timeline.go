package main

import (
	"database/sql"
	"sort"
	"strings"
	"time"
)

const pageSize = 20

// visibleSQL is the SQL condition for statuses (alias s) that viewer v may see.
func visibleSQL(v *Account) (string, []any) {
	if v == nil {
		return `s.visibility IN ('public','unlisted')`, nil
	}
	return `(s.visibility IN ('public','unlisted') OR s.account_id=?
		OR (s.visibility='private' AND EXISTS(SELECT 1 FROM follows f WHERE f.account_id=? AND f.target_id=s.account_id AND f.accepted=1))
		OR EXISTS(SELECT 1 FROM mentions m WHERE m.status_id=s.id AND m.account_id=?))
		AND NOT EXISTS(SELECT 1 FROM blocks b WHERE b.account_id=? AND b.target_id=s.account_id)`,
		[]any{v.ID, v.ID, v.ID, v.ID}
}

func viewerID(v *Account) string {
	if v == nil {
		return ""
	}
	return v.ID
}

// queryStatuses runs a timeline query. where/args further restrict the set.
func (app *App) queryStatuses(v *Account, where string, args []any, maxID string, limit int, asc bool) ([]*Status, error) {
	sts, err := app.queryStatusesNoQuotes(v, where, args, maxID, limit, asc)
	if err == nil {
		app.hydrateQuotes(v, sts)
	}
	return sts, err
}

func (app *App) queryStatusesNoQuotes(v *Account, where string, args []any, maxID string, limit int, asc bool) ([]*Status, error) {
	vis, vargs := visibleSQL(v)
	q := `SELECT ` + statusCols + ` FROM statuses s JOIN accounts a ON a.id=s.account_id WHERE a.suspended=0 AND ` + vis + ` AND (` + where + `)`
	all := append(append([]any{}, vargs...), args...)
	if maxID != "" {
		q += ` AND s.id < ?`
		all = append(all, maxID)
	}
	if asc {
		q += ` ORDER BY s.id ASC`
	} else {
		q += ` ORDER BY s.id DESC`
	}
	if limit > 0 {
		q += ` LIMIT ?`
		all = append(all, limit)
	}
	sts, err := app.scanStatuses(q, all...)
	if err != nil {
		return nil, err
	}
	return app.hydrate(v, sts)
}

func (app *App) scanStatuses(q string, args ...any) ([]*Status, error) {
	rows, err := app.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Status
	for rows.Next() {
		s, err := scanStatus(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (app *App) accountsByIDs(ids []string) (map[string]*Account, error) {
	out := map[string]*Account{}
	ids = uniq(ids)
	if len(ids) == 0 {
		return out, nil
	}
	q, args := inClause(`SELECT `+accountCols+` FROM accounts a WHERE a.id IN (%s)`, ids)
	accs, err := app.accountsQuery(q, args...)
	if err != nil {
		return nil, err
	}
	for _, a := range accs {
		out[a.ID] = a
	}
	return out, nil
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// hydrate loads accounts, boosted originals, attachments, mentions, tags,
// counters and the viewer's own likes/boosts for a list of statuses.
func (app *App) hydrate(v *Account, sts []*Status) ([]*Status, error) {
	if len(sts) == 0 {
		return sts, nil
	}
	// originals of boosts
	var boostIDs []string
	for _, s := range sts {
		if s.BoostOfID != "" {
			boostIDs = append(boostIDs, s.BoostOfID)
		}
	}
	originals := map[string]*Status{}
	if len(boostIDs) > 0 {
		q, args := inClause(`SELECT `+statusCols+` FROM statuses s WHERE s.id IN (%s)`, uniq(boostIDs))
		os, err := app.scanStatuses(q, args...)
		if err != nil {
			return nil, err
		}
		for _, o := range os {
			if o.Visibility == "public" || o.Visibility == "unlisted" || (v != nil && o.AccountID == v.ID) {
				originals[o.ID] = o
			}
		}
	}
	all := append([]*Status{}, sts...)
	for _, o := range originals {
		all = append(all, o)
	}
	var accIDs, stIDs []string
	for _, s := range all {
		accIDs = append(accIDs, s.AccountID, s.InReplyToAccountID)
		stIDs = append(stIDs, s.ID)
	}
	accs, err := app.accountsByIDs(accIDs)
	if err != nil {
		return nil, err
	}
	atts, err := app.attachmentsFor(stIDs)
	if err != nil {
		return nil, err
	}
	mentions := map[string][]*Account{}
	tags := map[string][]string{}
	likes, boosts, replies := map[string]int{}, map[string]int{}, map[string]int{}
	liked, boosted, bookmarked := map[string]bool{}, map[string]bool{}, map[string]bool{}
	{
		q, args := inClause(`SELECT m.status_id, `+accountCols+` FROM mentions m JOIN accounts a ON a.id=m.account_id WHERE m.status_id IN (%s)`, stIDs)
		rows, err := app.db.Query(q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var sid string
			a, err := scanAccount(prefixScanner{rows, &sid})
			if err == nil {
				mentions[sid] = append(mentions[sid], a)
			}
		}
		rows.Close()
	}
	countInto := func(q string, m map[string]int) {
		q, args := inClause(q, stIDs)
		rows, err := app.db.Query(q, args...)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var n int
			rows.Scan(&id, &n)
			m[id] = n
		}
	}
	countInto(`SELECT status_id, COUNT(*) FROM likes WHERE status_id IN (%s) GROUP BY status_id`, likes)
	countInto(`SELECT boost_of_id, COUNT(*) FROM statuses WHERE boost_of_id IN (%s) GROUP BY boost_of_id`, boosts)
	countInto(`SELECT in_reply_to_id, COUNT(*) FROM statuses WHERE in_reply_to_id IN (%s) GROUP BY in_reply_to_id`, replies)
	{
		q, args := inClause(`SELECT status_id, tag FROM status_tags WHERE status_id IN (%s)`, stIDs)
		if rows, err := app.db.Query(q, args...); err == nil {
			for rows.Next() {
				var id, t string
				rows.Scan(&id, &t)
				tags[id] = append(tags[id], t)
			}
			rows.Close()
		}
	}
	if v != nil {
		q, args := inClause(`SELECT status_id FROM likes WHERE account_id=? AND status_id IN (%s)`, stIDs)
		if rows, err := app.db.Query(q, append([]any{v.ID}, args...)...); err == nil {
			for rows.Next() {
				var id string
				rows.Scan(&id)
				liked[id] = true
			}
			rows.Close()
		}
		q, args = inClause(`SELECT status_id FROM bookmarks WHERE account_id=? AND status_id IN (%s)`, stIDs)
		if rows, err := app.db.Query(q, append([]any{v.ID}, args...)...); err == nil {
			for rows.Next() {
				var id string
				rows.Scan(&id)
				bookmarked[id] = true
			}
			rows.Close()
		}
		q, args = inClause(`SELECT boost_of_id FROM statuses WHERE account_id=? AND boost_of_id IN (%s)`, stIDs)
		if rows, err := app.db.Query(q, append([]any{v.ID}, args...)...); err == nil {
			for rows.Next() {
				var id string
				rows.Scan(&id)
				boosted[id] = true
			}
			rows.Close()
		}
	}
	votes := app.myVotes(v, stIDs)
	reactions := app.reactionsFor(v, stIDs)
	app.cardsFor(all)
	for _, s := range all {
		s.MyVotes = votes[s.ID]
		s.Reactions = reactions[s.ID]
		s.Account = accs[s.AccountID]
		s.InReplyToAccount = accs[s.InReplyToAccountID]
		s.Attachments = atts[s.ID]
		s.Mentions = mentions[s.ID]
		s.Tags = tags[s.ID]
		s.Likes, s.Boosts, s.Replies = likes[s.ID], boosts[s.ID], replies[s.ID]
		s.Liked, s.Boosted, s.Bookmarked = liked[s.ID], boosted[s.ID], bookmarked[s.ID]
	}
	out := sts[:0]
	for _, s := range sts {
		if s.Account == nil {
			continue
		}
		if s.BoostOfID != "" {
			s.BoostOf = originals[s.BoostOfID]
			if s.BoostOf == nil || s.BoostOf.Account == nil || s.BoostOf.Account.Suspended {
				continue
			}
			if v != nil && app.isBlockedBy(v.ID, s.BoostOf.AccountID) {
				continue
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// prefixScanner scans one extra leading column before an account row.
type prefixScanner struct {
	rows *sql.Rows
	dst  *string
}

func (p prefixScanner) Scan(dest ...any) error {
	return p.rows.Scan(append([]any{p.dst}, dest...)...)
}

// ---- timelines ----

func (app *App) homeTimeline(v *Account, maxID string) ([]*Status, error) {
	mute, margs := muteSQL(v)
	return app.queryStatuses(v, `(s.account_id=? OR s.account_id IN (SELECT target_id FROM follows WHERE account_id=? AND accepted=1)
		OR EXISTS(SELECT 1 FROM mentions m2 WHERE m2.status_id=s.id AND m2.account_id=?)) AND `+mute,
		append([]any{v.ID, v.ID, v.ID}, margs...), maxID, pageSize, false)
}

func (app *App) localTimeline(v *Account, maxID string) ([]*Status, error) {
	mute, margs := muteSQL(v)
	return app.queryStatuses(v, `s.local=1 AND s.visibility='public' AND s.boost_of_id='' AND `+mute, margs, maxID, pageSize, false)
}

func (app *App) federatedTimeline(v *Account, maxID string) ([]*Status, error) {
	mute, margs := muteSQL(v)
	return app.queryStatuses(v, `s.visibility='public' AND s.boost_of_id='' AND `+mute, margs, maxID, pageSize, false)
}

func (app *App) tagTimeline(v *Account, tag, maxID string) ([]*Status, error) {
	mute, margs := muteSQL(v)
	return app.queryStatuses(v, `s.visibility='public' AND s.id IN (SELECT status_id FROM status_tags WHERE tag=?) AND `+mute,
		append([]any{strings.ToLower(tag)}, margs...), maxID, pageSize, false)
}

func (app *App) accountTimeline(v *Account, acc *Account, withReplies, mediaOnly bool, maxID string) ([]*Status, error) {
	where := `s.account_id=?`
	if !withReplies {
		where += ` AND (s.in_reply_to_uri='' OR s.in_reply_to_account_id=s.account_id)`
	}
	if mediaOnly {
		where += ` AND s.boost_of_id='' AND EXISTS(SELECT 1 FROM attachments at WHERE at.status_id=s.id)`
	}
	return app.queryStatuses(v, where, []any{acc.ID}, maxID, pageSize, false)
}

func (app *App) bookmarksTimeline(v *Account, maxID string) ([]*Status, error) {
	return app.queryStatuses(v, `s.id IN (SELECT status_id FROM bookmarks WHERE account_id=?)`, []any{v.ID}, maxID, pageSize, false)
}

func (app *App) canSee(v *Account, st *Status) bool {
	sts, err := app.queryStatuses(v, `s.id=?`, []any{st.ID}, "", 1, false)
	return err == nil && len(sts) == 1
}

func (app *App) loadStatus(v *Account, id string) (*Status, error) {
	sts, err := app.queryStatuses(v, `s.id=?`, []any{id}, "", 1, false)
	if err != nil {
		return nil, err
	}
	if len(sts) == 0 {
		return nil, errNotFound
	}
	return sts[0], nil
}

// thread returns the visible ancestors (oldest first) and descendants
// (depth-first, with Depth set) of st.
func (app *App) thread(v *Account, st *Status) (anc, desc []*Status) {
	// Remote reply links can be malformed (including self-replies and
	// cycles). Never render a status twice or recurse without a bound.
	seen := map[string]bool{st.ID: true}
	cur := st
	for i := 0; i < 40 && cur.InReplyToID != ""; i++ {
		if seen[cur.InReplyToID] {
			break
		}
		p, err := app.loadStatus(v, cur.InReplyToID)
		if err != nil {
			break
		}
		seen[p.ID] = true
		anc = append([]*Status{p}, anc...)
		cur = p
	}
	rows, err := app.db.Query(`WITH RECURSIVE d(id, depth) AS (
		SELECT id, 1 FROM statuses WHERE in_reply_to_id=?
		UNION ALL SELECT s.id, d.depth+1 FROM statuses s JOIN d ON s.in_reply_to_id=d.id WHERE d.depth < 40)
		SELECT id FROM d LIMIT 500`, st.ID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	q, args := inClause(`s.id IN (%s)`, ids)
	sts, err := app.queryStatuses(v, q, args, "", 0, true)
	if err != nil {
		return
	}
	children := map[string][]*Status{}
	for _, s := range sts {
		children[s.InReplyToID] = append(children[s.InReplyToID], s)
	}
	for _, c := range children {
		sort.Slice(c, func(i, j int) bool { return c[i].ID < c[j].ID })
	}
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		if depth >= 40 {
			return
		}
		for _, c := range children[parent] {
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			c.Depth = depth
			desc = append(desc, c)
			walk(c.ID, depth+1)
		}
	}
	walk(st.ID, 0)
	return
}

// ---- notifications ----

type Notification struct {
	ID, Type  string
	From      *Account
	Status    *Status
	CreatedAt time.Time
	Read      bool
}

func (app *App) notifications(v *Account, maxID string) ([]*Notification, error) {
	where := `n.account_id=? AND a.suspended=0
		AND NOT EXISTS(SELECT 1 FROM mutes mu WHERE mu.account_id=n.account_id AND mu.target_id=n.from_account_id)`
	args := []any{v.ID}
	if maxID != "" {
		where += ` AND n.id < ?`
		args = append(args, maxID)
	}
	return app.notificationsWhere(v, where, args, "DESC", pageSize*2)
}

func (app *App) notificationsWhere(v *Account, where string, args []any, order string, limit int) ([]*Notification, error) {
	q := `SELECT n.id, n.type, n.from_account_id, n.status_id, n.created_at, n.read FROM notifications n
		JOIN accounts a ON a.id=n.from_account_id WHERE ` + where + ` ORDER BY n.id ` + order + ` LIMIT ?`
	args = append(args, limit)
	rows, err := app.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	var ns []*Notification
	var fromIDs, stIDs []string
	type raw struct{ from, status string }
	var raws []raw
	for rows.Next() {
		n := &Notification{}
		var from, sid string
		var created int64
		if err := rows.Scan(&n.ID, &n.Type, &from, &sid, &created, &n.Read); err != nil {
			rows.Close()
			return nil, err
		}
		n.CreatedAt = time.Unix(created, 0)
		ns = append(ns, n)
		raws = append(raws, raw{from, sid})
		fromIDs = append(fromIDs, from)
		if sid != "" {
			stIDs = append(stIDs, sid)
		}
	}
	rows.Close()
	accs, err := app.accountsByIDs(fromIDs)
	if err != nil {
		return nil, err
	}
	sts := map[string]*Status{}
	if len(stIDs) > 0 {
		q, args := inClause(`s.id IN (%s)`, uniq(stIDs))
		list, err := app.queryStatuses(v, q, args, "", 0, false)
		if err != nil {
			return nil, err
		}
		for _, s := range list {
			sts[s.ID] = s
		}
	}
	out := ns[:0]
	for i, n := range ns {
		n.From = accs[raws[i].from]
		if raws[i].status != "" {
			n.Status = sts[raws[i].status]
			if n.Status == nil {
				continue // deleted or no longer visible
			}
		}
		if n.From != nil {
			out = append(out, n)
		}
	}
	return out, nil
}

func (app *App) unreadNotifications(accountID string) int {
	return app.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=? AND read=0`, accountID)
}

func (app *App) followRequestCount(accountID string) int {
	return app.countInt(`SELECT COUNT(*) FROM follows WHERE target_id=? AND accepted=0`, accountID)
}

// followList returns accounts following (followers=true) or followed by acc.
func (app *App) followList(acc *Account, followers bool, pending bool, offset int) ([]*Account, error) {
	q := `SELECT ` + accountCols + ` FROM follows f JOIN accounts a ON a.id=f.account_id WHERE f.target_id=? AND f.accepted=? AND a.suspended=0 ORDER BY f.created_at DESC LIMIT 40 OFFSET ?`
	if !followers {
		q = `SELECT ` + accountCols + ` FROM follows f JOIN accounts a ON a.id=f.target_id WHERE f.account_id=? AND f.accepted=? AND a.suspended=0 ORDER BY f.created_at DESC LIMIT 40 OFFSET ?`
	}
	return app.accountsQuery(q, acc.ID, !pending, offset)
}
