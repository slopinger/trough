package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Things local users do. Each action updates the database and federates.

var visibilityRank = map[string]int{"public": 0, "unlisted": 1, "private": 2, "direct": 3}

type PostInput struct {
	Text, Spoiler, Visibility string
	Sensitive                 bool
	InReplyTo                 string // local status id
	MediaIDs                  []string
	Poll                      *Poll
	Language                  string    // ISO 639 code chosen by the client, may be empty
	QuoteOf                   string    // local status id of a quoted post
	QuotePolicy               string    // who may quote this post: public, followers, nobody ("" = public)
	PollStart                 time.Time // when the poll's duration was chosen (scheduled posts)
}

func (app *App) PostStatus(ctx context.Context, me *Account, in PostInput) (*Status, error) {
	text := strings.TrimSpace(in.Text)
	if utf8.RuneCountInString(text) > maxStatusChars {
		return nil, errors.New("post is too long")
	}
	if len(in.MediaIDs) > 4 {
		return nil, errors.New("at most 4 attachments")
	}
	if text == "" && len(in.MediaIDs) == 0 && in.Poll == nil {
		return nil, errors.New("post is empty")
	}
	if _, ok := visibilityRank[in.Visibility]; !ok {
		in.Visibility = "public"
	}
	var parent *Status
	if in.InReplyTo != "" {
		p, err := app.loadStatus(me, in.InReplyTo)
		if err != nil {
			return nil, errors.New("the post you are replying to is gone")
		}
		parent = p
		if visibilityRank[p.Visibility] > visibilityRank[in.Visibility] {
			in.Visibility = p.Visibility
		}
	}
	var quoted *Status
	if in.QuoteOf != "" {
		q, err := app.loadStatus(me, in.QuoteOf)
		if err != nil || app.quoteApproval(q, me) == "denied" {
			return nil, errors.New("that post can't be quoted")
		}
		if q.Visibility == "private" && visibilityRank[in.Visibility] < visibilityRank["private"] {
			in.Visibility = "private" // quoting my own followers-only post
		}
		quoted = q
	}
	if !quotePolicies[in.QuotePolicy] {
		in.QuotePolicy = ""
	}
	f := app.formatPost(ctx, text)
	if quoted != nil {
		// fallback for software that doesn't render quotes
		link := quoted.URL
		if link == "" {
			link = quoted.URI
		}
		f.HTML += `<p class="quote-inline">RE: ` + linkHTML(link) + `</p>`
	}
	id := newID()
	st := &Status{
		ID:         id,
		URI:        app.statusURI(me.Username, id),
		URL:        app.statusURL(me.Username, id),
		AccountID:  me.ID,
		Local:      true,
		Content:    f.HTML,
		Source:     text,
		Spoiler:    truncateRunes(strings.TrimSpace(in.Spoiler), 500),
		Sensitive:  in.Sensitive || strings.TrimSpace(in.Spoiler) != "",
		Visibility: in.Visibility,
		Language:   normLanguage(in.Language),
		CreatedAt:  time.Now(),
		Poll:       in.Poll,
		Emojis:     app.emojisIn(text, in.Spoiler),
	}
	st.QuotePolicy = in.QuotePolicy
	askQuote := false
	if quoted != nil {
		st.QuoteURI = quoted.URI
		if quoted.AccountID != me.ID {
			if quoted.Local {
				// approved on the spot; remote servers check the stamp
				stamp, err := app.authorizeQuote(quoted, st.URI, me)
				if err != nil {
					return nil, err
				}
				st.QuoteAuth = stamp
			} else if quotePolicy(quoted) != "" {
				// the quoted server knows FEP-044f: wait for its approval
				st.QuoteState, askQuote = "pending", true
			}
		}
	}
	if parent != nil {
		st.InReplyToURI, st.InReplyToID, st.InReplyToAccountID = parent.URI, parent.ID, parent.AccountID
	}
	if err := app.insertStatus(st); err != nil {
		return nil, err
	}
	for _, m := range f.Mentions {
		app.db.Exec(`INSERT OR IGNORE INTO mentions (status_id, account_id) VALUES (?,?)`, st.ID, m.ID)
	}
	for _, t := range f.Tags {
		app.db.Exec(`INSERT OR IGNORE INTO status_tags (status_id, tag) VALUES (?,?)`, st.ID, t)
	}
	for i, mid := range in.MediaIDs {
		app.db.Exec(`UPDATE attachments SET status_id=?, position=? WHERE id=? AND account_id=? AND status_id IS NULL`, st.ID, i, mid, me.ID)
	}
	full, err := app.loadStatus(me, st.ID)
	if err != nil {
		return nil, err
	}
	notified := map[string]bool{}
	for _, m := range f.Mentions {
		if m.IsLocal() {
			app.notify(m.ID, "mention", me.ID, st.ID)
			notified[m.ID] = true
		}
	}
	if parent != nil && parent.Account.IsLocal() && !notified[parent.AccountID] {
		app.notify(parent.AccountID, "mention", me.ID, st.ID)
	}
	inboxes := app.statusInboxes(full)
	if quoted != nil {
		if quoted.Account.IsLocal() {
			app.notify(quoted.AccountID, "quote", me.ID, st.ID)
		} else if st.Visibility == "public" || st.Visibility == "unlisted" {
			inboxes = append(inboxes, quoted.Account.DeliveryInbox())
		}
	}
	app.enqueue(me, inboxes, app.createActivity(full))
	if askQuote {
		// after the Create: Mastodon drops a QuoteRequest that it handles
		// while it is still storing the quoting post
		app.enqueueAt(me, []string{quoted.Account.DeliveryInbox()}, app.quoteRequestActivity(full, quoted), time.Now().Add(5*time.Second))
	}
	app.queueCard(full)
	app.publishStatus(full.ID, false)
	return full, nil
}

func (app *App) DeleteStatus(me *Account, st *Status) error {
	return app.deleteStatus(me, st, false)
}

// DeleteStatusKeepMedia deletes a post but keeps its uploads unattached
// for a day, so clients can "delete and redraft" with the same media.
func (app *App) DeleteStatusKeepMedia(me *Account, st *Status) error {
	return app.deleteStatus(me, st, true)
}

func (app *App) deleteStatus(me *Account, st *Status, keepMedia bool) error {
	if st.AccountID != me.ID {
		return errors.New("not your post")
	}
	if st.IsBoost() {
		return app.Unboost(me, st.BoostOf)
	}
	inboxes := app.statusInboxes(st)
	// also tell everyone who boosted it / servers that know it via boosts
	inboxes = append(inboxes, app.followerInboxesOfBoosters(st.ID)...)
	act := app.deleteActivity(st)
	if keepMedia {
		app.db.Exec(`UPDATE attachments SET status_id=NULL, created_at=? WHERE status_id=? AND account_id=?`, time.Now().Unix(), st.ID, me.ID)
	}
	if err := app.purgeStatus(st.ID); err != nil {
		return err
	}
	app.enqueue(me, inboxes, act)
	return nil
}

func (app *App) followerInboxesOfBoosters(statusID string) []string {
	rows, err := app.db.Query(`SELECT DISTINCT CASE WHEN a.shared_inbox != '' THEN a.shared_inbox ELSE a.inbox END
		FROM statuses s JOIN accounts a ON a.id=s.account_id WHERE s.boost_of_id=? AND a.domain != ''`, statusID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// purgeStatus deletes a status, its boosts, notifications and local media.
func (app *App) purgeStatus(id string) error {
	rows, err := app.db.Query(`SELECT file FROM attachments WHERE status_id=? AND file != ''`, id)
	if err == nil {
		var files []string
		for rows.Next() {
			var f string
			rows.Scan(&f)
			files = append(files, f)
		}
		rows.Close()
		for _, f := range files {
			os.Remove(filepath.Join(app.mediaDir, filepath.Base(f)))
		}
	}
	app.db.Exec(`DELETE FROM notifications WHERE status_id=? OR status_id IN (SELECT id FROM statuses WHERE boost_of_id=?)`, id, id)
	app.db.Exec(`DELETE FROM statuses WHERE boost_of_id=?`, id)
	app.db.Exec(`UPDATE statuses SET in_reply_to_id='' WHERE in_reply_to_id=?`, id)
	_, err = app.db.Exec(`DELETE FROM statuses WHERE id=?`, id)
	if err == nil {
		app.publishDelete(id)
	}
	return err
}

func (app *App) Like(me *Account, st *Status) error {
	if st.IsBoost() {
		st = st.BoostOf
	}
	likeID := newID()
	res, err := app.db.Exec(`INSERT OR IGNORE INTO likes (id,uri,account_id,status_id,created_at) VALUES (?,?,?,?,?)`,
		likeID, me.URI+"#likes/"+likeID, me.ID, st.ID, time.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if st.Account.IsLocal() {
		app.notify(st.AccountID, "favourite", me.ID, st.ID)
	} else {
		app.enqueue(me, []string{st.Account.DeliveryInbox()}, likeActivity(me, likeID, st))
	}
	return nil
}

func (app *App) Unlike(me *Account, st *Status) error {
	if st.IsBoost() {
		st = st.BoostOf
	}
	var likeID string
	if err := app.db.QueryRow(`SELECT id FROM likes WHERE account_id=? AND status_id=?`, me.ID, st.ID).Scan(&likeID); err != nil {
		return nil
	}
	app.db.Exec(`DELETE FROM likes WHERE id=?`, likeID)
	app.db.Exec(`DELETE FROM notifications WHERE type='favourite' AND from_account_id=? AND status_id=?`, me.ID, st.ID)
	if !st.Account.IsLocal() {
		app.enqueue(me, []string{st.Account.DeliveryInbox()}, undoActivity(me, likeActivity(me, likeID, st)))
	}
	return nil
}

func (app *App) Boost(me *Account, st *Status) error {
	if st.IsBoost() {
		st = st.BoostOf
	}
	if st.Visibility != "public" && st.Visibility != "unlisted" {
		return errors.New("only public posts can be boosted")
	}
	var x int
	if app.db.QueryRow(`SELECT 1 FROM statuses WHERE account_id=? AND boost_of_id=?`, me.ID, st.ID).Scan(&x) == nil {
		return nil
	}
	id := newID()
	b := &Status{
		ID: id, URI: app.statusURI(me.Username, id), URL: app.statusURL(me.Username, id),
		AccountID: me.ID, Local: true, BoostOfID: st.ID, Visibility: st.Visibility, CreatedAt: time.Now(),
		Account: me, BoostOf: st,
	}
	if err := app.insertStatus(b); err != nil {
		return err
	}
	app.publishStatus(b.ID, false)
	if st.Account.IsLocal() {
		app.notify(st.AccountID, "reblog", me.ID, st.ID)
	}
	inboxes := app.followerInboxes(me.ID)
	if !st.Account.IsLocal() {
		inboxes = append(inboxes, st.Account.DeliveryInbox())
	}
	app.enqueue(me, inboxes, app.announceActivity(b))
	return nil
}

func (app *App) Unboost(me *Account, st *Status) error {
	if st.IsBoost() {
		st = st.BoostOf
	}
	b, err := app.statusWhere(`s.account_id=? AND s.boost_of_id=?`, me.ID, st.ID)
	if err != nil {
		return nil
	}
	b.Account, b.BoostOf = me, st
	act := undoActivity(me, app.announceActivity(b))
	app.db.Exec(`DELETE FROM statuses WHERE id=?`, b.ID)
	app.db.Exec(`DELETE FROM notifications WHERE type='reblog' AND from_account_id=? AND status_id=?`, me.ID, st.ID)
	inboxes := app.followerInboxes(me.ID)
	if !st.Account.IsLocal() {
		inboxes = append(inboxes, st.Account.DeliveryInbox())
	}
	app.enqueue(me, inboxes, act)
	return nil
}

// ---- follows ----

func (app *App) Follow(me, target *Account) error {
	if me.ID == target.ID {
		return errors.New("you can't follow yourself")
	}
	if target.ActorType == "Application" && target.IsLocal() {
		return errors.New("that account can't be followed")
	}
	rel := app.relationship(me.ID, target.ID)
	if rel.Blocking || rel.BlockedBy {
		return errors.New("blocked")
	}
	if rel.Following || rel.Requested {
		if rel.Requested && !target.IsLocal() {
			// resend in case it got lost
			var uri string
			app.db.QueryRow(`SELECT uri FROM follows WHERE account_id=? AND target_id=?`, me.ID, target.ID).Scan(&uri)
			app.enqueue(me, []string{target.Inbox}, followActivity(uri, me, target))
		}
		return nil
	}
	id := newID()
	uri := me.URI + "#follows/" + id
	accepted := target.IsLocal() && !target.Locked
	if _, err := app.db.Exec(`INSERT INTO follows (id,uri,account_id,target_id,accepted,created_at) VALUES (?,?,?,?,?,?)`,
		id, uri, me.ID, target.ID, accepted, time.Now().Unix()); err != nil {
		return err
	}
	if target.IsLocal() {
		if accepted {
			app.notify(target.ID, "follow", me.ID, "")
		} else {
			app.notify(target.ID, "follow_request", me.ID, "")
		}
		return nil
	}
	app.enqueue(me, []string{target.Inbox}, followActivity(uri, me, target))
	return nil
}

func (app *App) Unfollow(me, target *Account) error {
	var uri string
	if err := app.db.QueryRow(`SELECT uri FROM follows WHERE account_id=? AND target_id=?`, me.ID, target.ID).Scan(&uri); err != nil {
		return nil
	}
	app.db.Exec(`DELETE FROM follows WHERE account_id=? AND target_id=?`, me.ID, target.ID)
	if !target.IsLocal() {
		app.enqueue(me, []string{target.Inbox}, undoActivity(me, followActivity(uri, me, target)))
	} else {
		app.db.Exec(`DELETE FROM notifications WHERE account_id=? AND from_account_id=? AND type IN ('follow','follow_request')`, target.ID, me.ID)
	}
	return nil
}

func (app *App) AcceptFollow(me, follower *Account) error {
	var uri string
	if err := app.db.QueryRow(`SELECT uri FROM follows WHERE account_id=? AND target_id=? AND accepted=0`, follower.ID, me.ID).Scan(&uri); err != nil {
		return errNotFound
	}
	app.db.Exec(`UPDATE follows SET accepted=1 WHERE account_id=? AND target_id=?`, follower.ID, me.ID)
	app.db.Exec(`DELETE FROM notifications WHERE account_id=? AND from_account_id=? AND type='follow_request'`, me.ID, follower.ID)
	app.notify(me.ID, "follow", follower.ID, "")
	if !follower.IsLocal() {
		app.enqueue(me, []string{follower.Inbox}, acceptRejectActivity("Accept", me, follower, uri))
	}
	return nil
}

// RejectFollow rejects a follow request or removes an existing follower.
func (app *App) RejectFollow(me, follower *Account) error {
	var uri string
	if err := app.db.QueryRow(`SELECT uri FROM follows WHERE account_id=? AND target_id=?`, follower.ID, me.ID).Scan(&uri); err != nil {
		return errNotFound
	}
	app.db.Exec(`DELETE FROM follows WHERE account_id=? AND target_id=?`, follower.ID, me.ID)
	app.db.Exec(`DELETE FROM notifications WHERE account_id=? AND from_account_id=? AND type='follow_request'`, me.ID, follower.ID)
	if !follower.IsLocal() {
		app.enqueue(me, []string{follower.Inbox}, acceptRejectActivity("Reject", me, follower, uri))
	}
	return nil
}

func (app *App) Block(me, target *Account) error {
	if me.ID == target.ID {
		return errors.New("you can't block yourself")
	}
	id := newID()
	uri := me.URI + "#blocks/" + id
	res, err := app.db.Exec(`INSERT OR IGNORE INTO blocks (id,uri,account_id,target_id,created_at) VALUES (?,?,?,?,?)`,
		id, uri, me.ID, target.ID, time.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	app.Unfollow(me, target)
	app.db.Exec(`DELETE FROM follows WHERE account_id=? AND target_id=?`, target.ID, me.ID)
	app.db.Exec(`DELETE FROM notifications WHERE account_id=? AND from_account_id=?`, me.ID, target.ID)
	if !target.IsLocal() {
		app.enqueue(me, []string{target.Inbox}, blockActivity(uri, me, target))
	}
	return nil
}

func (app *App) Unblock(me, target *Account) error {
	var uri string
	if err := app.db.QueryRow(`SELECT uri FROM blocks WHERE account_id=? AND target_id=?`, me.ID, target.ID).Scan(&uri); err != nil {
		return nil
	}
	app.db.Exec(`DELETE FROM blocks WHERE account_id=? AND target_id=?`, me.ID, target.ID)
	if !target.IsLocal() {
		app.enqueue(me, []string{target.Inbox}, undoActivity(me, blockActivity(uri, me, target)))
	}
	return nil
}

func (app *App) UpdateProfile(me *Account) error {
	if me.IsLocal() {
		var fieldText []string
		for _, f := range me.Fields {
			fieldText = append(fieldText, f.Name, f.Value)
		}
		me.Emojis = app.emojisIn(append([]string{me.DisplayName, me.NoteSource}, fieldText...)...)
	}
	if err := app.saveAccount(me); err != nil {
		return err
	}
	app.enqueue(me, app.followerInboxes(me.ID), app.updateActorActivity(me))
	return nil
}

var languageRe = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9]{2,8})?$`)

// normLanguage keeps a client's post language if it looks like a BCP 47
// tag (ISO 639 language plus an optional region or script).
func normLanguage(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !languageRe.MatchString(s) {
		return ""
	}
	return s
}
