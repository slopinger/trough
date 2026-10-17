package server

import (
	"errors"
	"strings"
	"time"

	"github.com/slopinger/trough/internal/ap"
	"github.com/slopinger/trough/internal/ulid"
)

// Polls are ActivityStreams Questions. Votes are Notes whose name is the
// chosen option, sent in reply to the Question (that's how Mastodon,
// Misskey and friends do it).

var pollDurations = map[string]time.Duration{
	"5m": 5 * time.Minute, "1h": time.Hour, "6h": 6 * time.Hour, "1d": 24 * time.Hour, "3d": 72 * time.Hour, "7d": 7 * 24 * time.Hour,
}

// newPoll validates poll input from the compose form.
func newPoll(options []string, duration string, multiple bool) (*Poll, error) {
	var opts []PollOption
	seen := map[string]bool{}
	for _, o := range options {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if len([]rune(o)) > 100 {
			return nil, errors.New("poll options are at most 100 characters")
		}
		if seen[strings.ToLower(o)] {
			return nil, errors.New("poll options must be different")
		}
		seen[strings.ToLower(o)] = true
		opts = append(opts, PollOption{Name: o})
	}
	if len(opts) == 0 {
		return nil, nil
	}
	if len(opts) < 2 {
		return nil, errors.New("a poll needs at least two options")
	}
	d, ok := pollDurations[duration]
	if !ok {
		d = 24 * time.Hour
	}
	return &Poll{Multiple: multiple, EndTime: time.Now().Add(d).UTC().Truncate(time.Second), Options: opts}, nil
}

func (p *Poll) Closed() bool { return !p.EndTime.IsZero() && time.Now().After(p.EndTime) }

func (p *Poll) Total() int {
	n := 0
	for _, o := range p.Options {
		n += o.Votes
	}
	return n
}

// questionFields adds Question members to a local note.
func (app *App) questionFields(st *Status, m obj) {
	p := st.Poll
	if p == nil {
		return
	}
	var opts []any
	for _, o := range p.Options {
		opts = append(opts, obj{"type": "Note", "name": o.Name, "replies": obj{"type": "Collection", "totalItems": o.Votes}})
	}
	m["type"] = "Question"
	if p.Multiple {
		m["anyOf"] = opts
	} else {
		m["oneOf"] = opts
	}
	m["endTime"] = ap.Time(p.EndTime)
	m["votersCount"] = app.countInt(`SELECT COUNT(DISTINCT account_id) FROM poll_votes WHERE status_id=?`, st.ID)
	if p.Closed() {
		m["closed"] = ap.Time(p.EndTime)
	}
}

// Vote records a local user's vote and federates it for remote polls.
func (app *App) Vote(me *Account, st *Status, choices []int) error {
	if st.IsBoost() {
		st = st.BoostOf
	}
	p := st.Poll
	if p == nil {
		return errors.New("this post has no poll")
	}
	if p.Closed() {
		return errors.New("the poll has ended")
	}
	if st.AccountID == me.ID {
		return errors.New("you can't vote in your own poll")
	}
	if app.countInt(`SELECT COUNT(*) FROM poll_votes WHERE status_id=? AND account_id=?`, st.ID, me.ID) > 0 {
		return errors.New("you already voted")
	}
	if len(choices) == 0 || (!p.Multiple && len(choices) > 1) {
		return errors.New("pick an option")
	}
	seen := map[int]bool{}
	for _, c := range choices {
		if c < 0 || c >= len(p.Options) || seen[c] {
			return errors.New("invalid choice")
		}
		seen[c] = true
	}
	author, err := app.AccountByID(st.AccountID)
	if err != nil {
		return err
	}
	for _, c := range choices {
		uri := me.URI + "#votes/" + ulid.New()
		app.db.Exec(`INSERT OR IGNORE INTO poll_votes (status_id, account_id, choice, uri, created_at) VALUES (?,?,?,?,?)`,
			st.ID, me.ID, c, uri, time.Now().Unix())
		p.Options[c].Votes++
		if !author.IsLocal() {
			note := obj{"id": uri, "type": "Note", "name": p.Options[c].Name, "attributedTo": me.URI, "inReplyTo": st.URI,
				"to": []any{author.URI}, "published": ap.Time(time.Now())}
			app.enqueue(me, []string{author.DeliveryInbox()}, withContext(obj{
				"id": uri + "/activity", "type": "Create", "actor": me.URI, "to": []any{author.URI}, "object": note,
			}))
		}
	}
	if err := app.savePollCounts(st); err != nil {
		return err
	}
	if author.IsLocal() {
		app.pollChanged(st)
	}
	return nil
}

func (app *App) savePollCounts(st *Status) error {
	b, err := jsonMarshal(st.Poll)
	if err != nil {
		return err
	}
	_, err = app.db.Exec(`UPDATE statuses SET poll=? WHERE id=?`, b, st.ID)
	return err
}

// pollChanged tells followers and voters the new counts of a local poll.
func (app *App) pollChanged(st *Status) {
	full, err := app.StatusByID(st.ID)
	if err != nil {
		return
	}
	sts, err := app.hydrate(nil, []*Status{full})
	if err != nil || len(sts) == 0 {
		return
	}
	full = sts[0]
	inboxes := app.statusInboxes(full)
	rows, err := app.db.Query(`SELECT DISTINCT CASE WHEN a.shared_inbox != '' THEN a.shared_inbox ELSE a.inbox END
		FROM poll_votes v JOIN accounts a ON a.id=v.account_id WHERE v.status_id=? AND a.domain != ''`, st.ID)
	if err == nil {
		for rows.Next() {
			var s string
			rows.Scan(&s)
			inboxes = append(inboxes, s)
		}
		rows.Close()
	}
	app.enqueue(full.Account, inboxes, app.updateNoteActivity(full))
}

// inPollVote handles a remote vote on a local poll. It reports whether the
// note was a vote (and must not be stored as a post).
func (app *App) inPollVote(voter *Account, note obj) bool {
	name := ap.Str(note, "name")
	parent := ap.ID(note["inReplyTo"])
	if name == "" || parent == "" || !app.isLocalIRI(parent) || strings.TrimSpace(plainText(ap.Str(note, "content"))) != "" {
		return false
	}
	st, err := app.StatusByURI(parent)
	if err != nil || st.Poll == nil {
		return false
	}
	if st.Poll.Closed() {
		return true
	}
	for i, o := range st.Poll.Options {
		if o.Name != name {
			continue
		}
		var already int
		app.db.QueryRow(`SELECT COUNT(*) FROM poll_votes WHERE status_id=? AND account_id=?`, st.ID, voter.ID).Scan(&already)
		if already > 0 && !st.Poll.Multiple {
			return true
		}
		res, err := app.db.Exec(`INSERT OR IGNORE INTO poll_votes (status_id, account_id, choice, uri, created_at) VALUES (?,?,?,?,?)`,
			st.ID, voter.ID, i, ap.Str(note, "id"), time.Now().Unix())
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				st.Poll.Options[i].Votes++
				app.savePollCounts(st)
				app.pollChanged(st)
			}
		}
		return true
	}
	return true
}

// myVotes returns the poll choices the viewer made, keyed by status id.
func (app *App) myVotes(v *Account, ids []string) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	if v == nil || len(ids) == 0 {
		return out
	}
	q, args := inClause(`SELECT status_id, choice FROM poll_votes WHERE account_id=? AND status_id IN (%s)`, ids)
	rows, err := app.db.Query(q, append([]any{v.ID}, args...)...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c int
		rows.Scan(&id, &c)
		if out[id] == nil {
			out[id] = map[int]bool{}
		}
		out[id][c] = true
	}
	return out
}

// closeEndedPolls notifies authors and local voters once a poll ends.
func (app *App) closeEndedPolls() {
	rows, err := app.db.Query(`SELECT id, account_id, poll FROM statuses WHERE poll != '' AND id NOT IN (SELECT status_id FROM notifications WHERE type='poll')`)
	if err != nil {
		return
	}
	type ended struct{ id, author string }
	var list []ended
	for rows.Next() {
		var id, author, raw string
		rows.Scan(&id, &author, &raw)
		var p Poll
		if jsonUnmarshal([]byte(raw), &p) == nil && p.Closed() && time.Since(p.EndTime) < 7*24*time.Hour {
			list = append(list, ended{id, author})
		}
	}
	rows.Close()
	for _, e := range list {
		voters, _ := app.accountsQuery(`SELECT DISTINCT `+accountCols+` FROM poll_votes v JOIN accounts a ON a.id=v.account_id WHERE v.status_id=? AND a.domain=''`, e.id)
		notified := false
		for _, v := range voters {
			app.db.Exec(`INSERT INTO notifications (id,account_id,type,from_account_id,status_id,created_at) VALUES (?,?,'poll',?,?,?)`,
				ulid.New(), v.ID, e.author, e.id, time.Now().Unix())
			notified = true
		}
		if a, err := app.AccountByID(e.author); err == nil && a.IsLocal() {
			app.db.Exec(`INSERT INTO notifications (id,account_id,type,from_account_id,status_id,created_at) VALUES (?,?,'poll',?,?,?)`,
				ulid.New(), a.ID, a.ID, e.id, time.Now().Unix())
			notified = true
			if st, err := app.StatusByID(e.id); err == nil {
				app.pollChanged(st) // final counts, now "closed"
			}
		}
		if !notified {
			// remember that this poll is done so it isn't checked again
			app.db.Exec(`INSERT INTO notifications (id,account_id,type,from_account_id,status_id,created_at,read) VALUES (?,?,'poll',?,?,?,1)`,
				ulid.New(), e.author, e.author, e.id, time.Now().Unix())
		}
	}
}
