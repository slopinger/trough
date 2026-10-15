package main

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ---- editing ----

// EditStatus changes the text of a local post and federates an Update.
// Visibility, attachments and polls stay as they are.
func (app *App) EditStatus(ctx context.Context, me *Account, st *Status, text, spoiler string, sensitive bool) (*Status, error) {
	if st.AccountID != me.ID || !st.Local || st.IsBoost() {
		return nil, errors.New("you can only edit your own posts")
	}
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > maxStatusChars {
		return nil, errors.New("post is too long")
	}
	if text == "" && len(st.Attachments) == 0 {
		return nil, errors.New("post is empty")
	}
	old, err := app.loadStatus(me, st.ID)
	if err != nil {
		return nil, err
	}
	oldInboxes := app.statusInboxes(old)
	f := app.formatPost(ctx, text)
	st.Content, st.Source = f.HTML, text
	st.Spoiler = truncateRunes(strings.TrimSpace(spoiler), 500)
	st.Sensitive = sensitive || st.Spoiler != ""
	st.Emojis = app.emojisIn(text, st.Spoiler)
	if err := app.updateStatusContent(st); err != nil {
		return nil, err
	}
	wasMentioned := map[string]bool{}
	for _, m := range old.Mentions {
		wasMentioned[m.ID] = true
	}
	app.db.Exec(`DELETE FROM mentions WHERE status_id=?`, st.ID)
	app.db.Exec(`DELETE FROM status_tags WHERE status_id=?`, st.ID)
	for _, m := range f.Mentions {
		app.db.Exec(`INSERT OR IGNORE INTO mentions (status_id, account_id) VALUES (?,?)`, st.ID, m.ID)
		if m.IsLocal() && !wasMentioned[m.ID] {
			app.notify(m.ID, "mention", me.ID, st.ID)
		}
	}
	for _, t := range f.Tags {
		app.db.Exec(`INSERT OR IGNORE INTO status_tags (status_id, tag) VALUES (?,?)`, st.ID, t)
	}
	full, err := app.loadStatus(me, st.ID)
	if err != nil {
		return nil, err
	}
	full.CardURL = ""
	app.db.Exec(`UPDATE statuses SET card_url='' WHERE id=?`, st.ID)
	app.queueCard(full)
	// everyone who saw the old version, plus anyone newly mentioned
	app.enqueue(me, append(oldInboxes, app.statusInboxes(full)...), app.updateNoteActivity(full))
	app.publishStatus(full.ID, true)
	return full, nil
}

// ---- pinned posts ----

const maxPins = 5

func (app *App) Pin(me *Account, st *Status) error {
	if st.AccountID != me.ID || st.IsBoost() {
		return errors.New("you can only pin your own posts")
	}
	if st.Visibility != "public" && st.Visibility != "unlisted" {
		return errors.New("only public and unlisted posts can be pinned")
	}
	if !st.PinnedAt.IsZero() {
		return nil
	}
	if app.countInt(`SELECT COUNT(*) FROM statuses WHERE account_id=? AND pinned_at > 0`, me.ID) >= maxPins {
		return errors.New("you can pin at most 5 posts")
	}
	app.db.Exec(`UPDATE statuses SET pinned_at=? WHERE id=?`, time.Now().Unix(), st.ID)
	app.enqueue(me, app.followerInboxes(me.ID), app.collectionActivity("Add", me, st))
	return nil
}

func (app *App) Unpin(me *Account, st *Status) error {
	if st.AccountID != me.ID || st.PinnedAt.IsZero() {
		return nil
	}
	app.db.Exec(`UPDATE statuses SET pinned_at=0 WHERE id=?`, st.ID)
	app.enqueue(me, app.followerInboxes(me.ID), app.collectionActivity("Remove", me, st))
	return nil
}

func (app *App) collectionActivity(kind string, me *Account, st *Status) obj {
	return withContext(obj{
		"id":     me.URI + "#" + strings.ToLower(kind) + "/" + newID(),
		"type":   kind,
		"actor":  me.URI,
		"to":     []any{asPublic},
		"cc":     []any{me.FollowersURI},
		"object": st.URI,
		"target": me.URI + "/collections/featured",
	})
}

func (app *App) pinnedStatuses(v, acc *Account) []*Status {
	sts, _ := app.queryStatuses(v, `s.account_id=? AND s.pinned_at > 0`, []any{acc.ID}, "", maxPins*2, false)
	// newest pin first
	for i := 1; i < len(sts); i++ {
		for j := i; j > 0 && sts[j].PinnedAt.After(sts[j-1].PinnedAt); j-- {
			sts[j], sts[j-1] = sts[j-1], sts[j]
		}
	}
	return sts
}

// inAddRemove handles pins of remote accounts (Add/Remove to their featured collection).
func (app *App) inAddRemove(ctx context.Context, actor *Account, act obj) error {
	target := idOf(act["target"])
	if target == "" || (target != actor.FeaturedURI && !strings.HasSuffix(target, "/collections/featured")) || !sameHost(target, actor.URI) {
		return nil
	}
	iri := idOf(act["object"])
	st, err := app.StatusByURI(iri)
	if err != nil {
		if typeOf(act) == "Remove" || !app.hasLocalFollowers(actor.ID) {
			return nil
		}
		if st, err = app.resolveStatus(ctx, iri); err != nil {
			return err
		}
	}
	if st.AccountID != actor.ID {
		return errors.New("pin of someone else's post")
	}
	if typeOf(act) == "Add" {
		_, err = app.db.Exec(`UPDATE statuses SET pinned_at=? WHERE id=?`, time.Now().Unix(), st.ID)
	} else {
		_, err = app.db.Exec(`UPDATE statuses SET pinned_at=0 WHERE id=?`, st.ID)
	}
	return err
}

// syncFeatured fetches a remote account's pinned posts.
func (app *App) syncFeatured(ctx context.Context, acc *Account) {
	if acc.IsLocal() || acc.FeaturedURI == "" || !sameHost(acc.FeaturedURI, acc.URI) {
		return
	}
	coll, err := app.fetchAP(ctx, acc.FeaturedURI)
	if err != nil {
		return
	}
	items := coll["orderedItems"]
	if items == nil {
		items = coll["items"]
	}
	if items == nil {
		if first, ok := coll["first"].(obj); ok {
			items = first["orderedItems"]
		}
	}
	var pinned []string
	list, _ := items.([]any)
	for i, it := range list {
		if i >= 10 {
			break
		}
		var st *Status
		if note, ok := it.(obj); ok && noteTypes[typeOf(note)] && attributedToIncludes(note, acc.URI) && sameHost(str(note, "id"), acc.URI) {
			if v := visibilityFrom(note, acc.FollowersURI); v != "public" && v != "unlisted" {
				continue
			}
			st, err = app.storeNote(ctx, acc, note, 5)
		} else if iri := idOf(it); sameHost(iri, acc.URI) {
			st, err = app.resolveStatusDepth(ctx, iri, 5)
		}
		if err == nil && st != nil && st.AccountID == acc.ID {
			pinned = append(pinned, st.ID)
		}
	}
	app.db.Exec(`UPDATE statuses SET pinned_at=0 WHERE account_id=? AND pinned_at > 0`, acc.ID)
	now := time.Now().Unix()
	for i, id := range pinned {
		app.db.Exec(`UPDATE statuses SET pinned_at=? WHERE id=?`, now-int64(i), id)
	}
}

// ---- bookmarks ----

func (app *App) Bookmark(me *Account, st *Status) error {
	_, err := app.db.Exec(`INSERT OR IGNORE INTO bookmarks (account_id, status_id, created_at) VALUES (?,?,?)`, me.ID, st.ID, time.Now().Unix())
	return err
}

func (app *App) Unbookmark(me *Account, st *Status) error {
	_, err := app.db.Exec(`DELETE FROM bookmarks WHERE account_id=? AND status_id=?`, me.ID, st.ID)
	return err
}

// ---- mutes ----

func (app *App) Mute(me, target *Account) error {
	if me.ID == target.ID {
		return errors.New("you can't mute yourself")
	}
	_, err := app.db.Exec(`INSERT OR IGNORE INTO mutes (account_id, target_id, created_at) VALUES (?,?,?)`, me.ID, target.ID, time.Now().Unix())
	return err
}

func (app *App) Unmute(me, target *Account) error {
	_, err := app.db.Exec(`DELETE FROM mutes WHERE account_id=? AND target_id=?`, me.ID, target.ID)
	return err
}

func (app *App) isMuting(me, other string) bool {
	var x int
	return app.db.QueryRow(`SELECT 1 FROM mutes WHERE account_id=? AND target_id=?`, me, other).Scan(&x) == nil
}

// muteSQL hides posts (and boosts of posts) by accounts the viewer muted.
func muteSQL(v *Account) (string, []any) {
	if v == nil {
		return "1", nil
	}
	return `NOT EXISTS(SELECT 1 FROM mutes mu WHERE mu.account_id=? AND mu.target_id=s.account_id)
		AND NOT (s.boost_of_id != '' AND EXISTS(SELECT 1 FROM statuses bo JOIN mutes mu2 ON mu2.target_id=bo.account_id
			WHERE bo.id=s.boost_of_id AND mu2.account_id=?))`, []any{v.ID, v.ID}
}

// ---- keyword filters ----

type Filter struct {
	ID, Phrase, Action string
	WholeWord          bool
	re                 *regexp.Regexp
}

func (app *App) filtersFor(accountID string) []*Filter {
	rows, err := app.db.Query(`SELECT id, phrase, whole_word, action FROM filters WHERE account_id=? ORDER BY created_at`, accountID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*Filter
	for rows.Next() {
		f := &Filter{}
		rows.Scan(&f.ID, &f.Phrase, &f.WholeWord, &f.Action)
		pat := regexp.QuoteMeta(strings.ToLower(f.Phrase))
		if f.WholeWord {
			pat = `(^|[^\pL\pN_])` + pat + `($|[^\pL\pN_])`
		}
		if re, err := regexp.Compile(pat); err == nil {
			f.re = re
			out = append(out, f)
		}
	}
	return out
}

func (f *Filter) matches(st *Status) bool {
	text := strings.ToLower(plainText(st.Content) + " " + st.Spoiler)
	for _, t := range st.Tags {
		text += " #" + t
	}
	return f.re.MatchString(text)
}

// applyFilters drops posts matching "hide" filters and marks posts matching
// "warn" filters so they are shown collapsed. Own posts are never filtered.
func (app *App) applyFilters(v *Account, sts []*Status) []*Status {
	if v == nil {
		return sts
	}
	filters := app.filtersFor(v.ID)
	if len(filters) == 0 {
		return sts
	}
	out := sts[:0]
	for _, s := range sts {
		o := s
		if s.BoostOf != nil {
			o = s.BoostOf
		}
		hidden := false
		if o.AccountID != v.ID {
			for _, f := range filters {
				if f.matches(o) {
					if f.Action == "hide" {
						hidden = true
					} else if s.FilteredBy == "" {
						s.FilteredBy = f.Phrase
					}
				}
			}
		}
		if !hidden {
			out = append(out, s)
		}
	}
	return out
}

func (app *App) AddFilter(me *Account, phrase string, wholeWord bool, action string) error {
	phrase = strings.TrimSpace(phrase)
	if phrase == "" || utf8.RuneCountInString(phrase) > 100 {
		return errors.New("filter phrases are 1-100 characters")
	}
	if action != "hide" {
		action = "warn"
	}
	if app.countInt(`SELECT COUNT(*) FROM filters WHERE account_id=?`, me.ID) >= 100 {
		return errors.New("too many filters")
	}
	_, err := app.db.Exec(`INSERT INTO filters (id, account_id, phrase, whole_word, action, created_at) VALUES (?,?,?,?,?,?)`,
		newID(), me.ID, phrase, wholeWord, action, time.Now().Unix())
	return err
}

// ---- reports ----

type Report struct {
	ID, Comment         string
	Reporter, Target    *Account
	Statuses            []*Status
	Forwarded, Remote   bool
	CreatedAt, Resolved time.Time
}

// FileReport stores a report by a local user and optionally forwards it to
// the target's server as a Flag from the instance actor (like Mastodon, so
// the reporter stays anonymous to the other server).
func (app *App) FileReport(me, target *Account, statusIDs []string, comment string, forward bool) error {
	if me.ID == target.ID {
		return errors.New("you can't report yourself")
	}
	var uris []string
	var keep []string
	for _, id := range statusIDs {
		if st, err := app.StatusByID(id); err == nil && st.AccountID == target.ID {
			keep = append(keep, st.ID)
			uris = append(uris, st.URI)
		}
	}
	ids, _ := jsonMarshal(keep)
	id := newID()
	comment = truncateRunes(strings.TrimSpace(comment), 1000)
	forwarded := forward && !target.IsLocal()
	if _, err := app.db.Exec(`INSERT INTO reports (id,uri,account_id,target_id,status_ids,comment,forwarded,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		id, app.cfg.BaseURL()+"/reports/"+id, me.ID, target.ID, ids, comment, forwarded, time.Now().Unix()); err != nil {
		return err
	}
	app.notifyAdmins(me.ID, "report")
	if forwarded {
		inst := app.instanceActor()
		objects := append([]any{target.URI}, toAny(uris)...)
		app.enqueue(inst, []string{target.Inbox}, withContext(obj{
			"id": app.cfg.BaseURL() + "/reports/" + id, "type": "Flag", "actor": inst.URI, "content": comment, "object": objects,
		}))
	}
	return nil
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func (app *App) notifyAdmins(fromID, typ string) {
	rows, err := app.db.Query(`SELECT account_id FROM users WHERE admin=1`)
	if err != nil {
		return
	}
	var admins []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		admins = append(admins, id)
	}
	rows.Close()
	for _, a := range admins {
		app.db.Exec(`INSERT INTO notifications (id,account_id,type,from_account_id,status_id,created_at) VALUES (?,?,?,?,'',?)`,
			newID(), a, typ, fromID, time.Now().Unix())
	}
}

// inFlag stores reports other servers send about our users.
func (app *App) inFlag(actor *Account, act obj) error {
	var target *Account
	var statusIDs []string
	for _, iri := range idsOf(act["object"]) {
		if acc, err := app.localTarget(iri); err == nil && target == nil {
			target = acc
		} else if st, err := app.StatusByURI(iri); err == nil && st.Local {
			statusIDs = append(statusIDs, st.ID)
			if target == nil {
				target, _ = app.AccountByID(st.AccountID)
			}
		}
	}
	if target == nil {
		return nil
	}
	ids, _ := jsonMarshal(statusIDs)
	_, err := app.db.Exec(`INSERT INTO reports (id,uri,account_id,target_id,status_ids,comment,created_at) VALUES (?,?,?,?,?,?,?)`,
		newID(), str(act, "id"), actor.ID, target.ID, ids, truncateRunes(plainText(str(act, "content")), 1000), time.Now().Unix())
	if err == nil {
		app.notifyAdmins(actor.ID, "report")
	}
	return err
}

func (app *App) openReports() []*Report {
	rows, err := app.db.Query(`SELECT id, account_id, target_id, status_ids, comment, forwarded, created_at FROM reports WHERE resolved_at=0 ORDER BY created_at DESC LIMIT 50`)
	if err != nil {
		return nil
	}
	type raw struct {
		r                  *Report
		from, target, sids string
	}
	var list []raw
	for rows.Next() {
		r := &Report{}
		var x raw
		var created int64
		rows.Scan(&r.ID, &x.from, &x.target, &x.sids, &r.Comment, &r.Forwarded, &created)
		r.CreatedAt = time.Unix(created, 0)
		x.r = r
		list = append(list, x)
	}
	rows.Close()
	var out []*Report
	for _, x := range list {
		x.r.Reporter, _ = app.AccountByID(x.from)
		x.r.Target, _ = app.AccountByID(x.target)
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

// ---- edit history ----

type StatusVersion struct {
	Content, Spoiler string
	Sensitive        bool
	At               time.Time
}

// history returns all versions of a status, oldest first, ending with the current one.
func (app *App) history(st *Status) []StatusVersion {
	var out []StatusVersion
	rows, err := app.db.Query(`SELECT content, spoiler, sensitive, created_at FROM status_edits WHERE status_id=? ORDER BY created_at, id`, st.ID)
	if err == nil {
		for rows.Next() {
			var v StatusVersion
			var at int64
			rows.Scan(&v.Content, &v.Spoiler, &v.Sensitive, &at)
			v.At = time.Unix(at, 0)
			out = append(out, v)
		}
		rows.Close()
	}
	at := st.EditedAt
	if at.IsZero() {
		at = st.CreatedAt
	}
	return append(out, StatusVersion{Content: st.Content, Spoiler: st.Spoiler, Sensitive: st.Sensitive, At: at})
}
