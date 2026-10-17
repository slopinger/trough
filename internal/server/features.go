package server

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/slopinger/trough/internal/ap"
	"github.com/slopinger/trough/internal/ulid"
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
	st.Sensitive = sensitive || st.Spoiler != "" || (me.ForceSensitive && len(st.Attachments) > 0)
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
	app.notifyEdited(full)
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
		"id":     me.URI + "#" + strings.ToLower(kind) + "/" + ulid.New(),
		"type":   kind,
		"actor":  me.URI,
		"to":     []any{ap.Public},
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
	target := ap.ID(act["target"])
	if target == "" || (target != actor.FeaturedURI && !strings.HasSuffix(target, "/collections/featured")) || !ap.SameHost(target, actor.URI) {
		return nil
	}
	iri := ap.ID(act["object"])
	st, err := app.StatusByURI(iri)
	if err != nil {
		if ap.Type(act) == "Remove" || !app.hasLocalFollowers(actor.ID) {
			return nil
		}
		if st, err = app.resolveStatus(ctx, iri); err != nil {
			return err
		}
	}
	if st.AccountID != actor.ID {
		return errors.New("pin of someone else's post")
	}
	if ap.Type(act) == "Add" {
		_, err = app.db.Exec(`UPDATE statuses SET pinned_at=? WHERE id=?`, time.Now().Unix(), st.ID)
	} else {
		_, err = app.db.Exec(`UPDATE statuses SET pinned_at=0 WHERE id=?`, st.ID)
	}
	return err
}

// syncFeatured fetches a remote account's pinned posts.
func (app *App) syncFeatured(ctx context.Context, acc *Account) {
	if acc.IsLocal() || acc.FeaturedURI == "" || !ap.SameHost(acc.FeaturedURI, acc.URI) {
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
		if note, ok := it.(obj); ok && noteTypes[ap.Type(note)] && attributedToIncludes(note, acc.URI) && ap.SameHost(ap.Str(note, "id"), acc.URI) {
			if v := ap.Visibility(note, acc.FollowersURI); v != "public" && v != "unlisted" {
				continue
			}
			st, err = app.storeNote(ctx, acc, note, 5)
		} else if iri := ap.ID(it); ap.SameHost(iri, acc.URI) {
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

func (app *App) Mute(me, target *Account) error { return app.MuteFor(me, target, true, 0) }

// MuteFor mutes target, like Mastodon: notifications says whether their
// notifications are hidden too, and a duration above zero lifts the mute
// after that long. Muting again replaces both.
func (app *App) MuteFor(me, target *Account, notifications bool, d time.Duration) error {
	if me.ID == target.ID {
		return errors.New("you can't mute yourself")
	}
	var expires int64
	if d > 0 {
		expires = time.Now().Add(d).Unix()
	}
	_, err := app.db.Exec(`INSERT INTO mutes (account_id, target_id, created_at, expires_at, notifications) VALUES (?,?,?,?,?)
		ON CONFLICT(account_id, target_id) DO UPDATE SET expires_at=excluded.expires_at, notifications=excluded.notifications`,
		me.ID, target.ID, time.Now().Unix(), expires, notifications)
	return err
}

// muteExpiry maps the accounts me muted for a while to the end of the mute.
func (app *App) muteExpiry(me *Account) map[string]time.Time {
	out := map[string]time.Time{}
	rows, err := app.db.Query(`SELECT target_id, expires_at FROM mutes WHERE account_id=? AND expires_at > 0`, me.ID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var exp int64
		if rows.Scan(&id, &exp) == nil {
			out[id] = time.Unix(exp, 0)
		}
	}
	return out
}

// muteNotes describes the settings page's mutes that aren't plain
// indefinite ones: until when, and whether notifications still come through.
func (app *App) muteNotes(me *Account) map[string]string {
	out := map[string]string{}
	rows, err := app.db.Query(`SELECT target_id, expires_at, notifications FROM mutes WHERE account_id=?`, me.ID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var exp int64
		var notif bool
		if rows.Scan(&id, &exp, &notif) != nil {
			continue
		}
		var parts []string
		if exp > 0 {
			parts = append(parts, "muted until "+time.Unix(exp, 0).UTC().Format("Mon, 2 Jan 2006 15:04 MST"))
		}
		if !notif {
			parts = append(parts, "posts only, notifications still shown")
		}
		if len(parts) > 0 {
			out[id] = strings.Join(parts, " · ")
		}
	}
	return out
}

// expireMutes lifts timed mutes whose time is up (called every minute).
func (app *App) expireMutes(now time.Time) {
	app.db.Exec(`DELETE FROM mutes WHERE expires_at > 0 AND expires_at <= ?`, now.Unix())
}

// muteDuration reads a mute duration in seconds; Mastodon's web UI offers
// up to 7 days, the API takes any length. 0 (or nothing) means indefinitely.
func muteDuration(s string) time.Duration {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 || n > 10*365*24*3600 {
		return 0
	}
	return time.Duration(n) * time.Second
}

func (app *App) Unmute(me, target *Account) error {
	_, err := app.db.Exec(`DELETE FROM mutes WHERE account_id=? AND target_id=?`, me.ID, target.ID)
	return err
}

func (app *App) isMuting(me, other string) bool {
	var x int
	return app.db.QueryRow(`SELECT 1 FROM mutes WHERE account_id=? AND target_id=?`, me, other).Scan(&x) == nil
}

// muteSQL hides posts (and boosts of posts) by accounts the viewer muted
// and from servers they blocked.
func muteSQL(v *Account) (string, []any) {
	limited, largs := limitedSQL(v)
	if v == nil {
		return limited, largs
	}
	return `NOT EXISTS(SELECT 1 FROM mutes mu WHERE mu.account_id=? AND mu.target_id=s.account_id)
		AND NOT (s.boost_of_id != '' AND EXISTS(SELECT 1 FROM statuses bo JOIN mutes mu2 ON mu2.target_id=bo.account_id
			WHERE bo.id=s.boost_of_id AND mu2.account_id=?)) AND ` + domainBlockSQL + ` AND ` + limited, append([]any{v.ID, v.ID, v.ID, v.ID}, largs...)
}

// ---- keyword filters ----

// A filter group (Mastodon's v2 Filter) has a title, contexts, an action and
// an optional expiry; its keywords are the rows of filters. The web UI makes
// one group per phrase.

var filterContexts = []string{"home", "notifications", "public", "thread", "account"}

type FilterGroup struct {
	ID, Title, Action string
	Context           []string // empty: everywhere
	ExpiresAt         int64    // unix seconds, 0: never
	Keywords          []*Filter
}

func (g *FilterGroup) Contexts() []string {
	if len(g.Context) == 0 {
		return filterContexts
	}
	return g.Context
}

func (g *FilterGroup) expired() bool { return g.ExpiresAt > 0 && g.ExpiresAt <= time.Now().Unix() }

// Filter is one keyword of a group.
type Filter struct {
	ID, Phrase, Action string // Action is the group's
	WholeWord          bool
	Group              *FilterGroup
	re                 *regexp.Regexp
}

func (f *Filter) compile() bool {
	pat := regexp.QuoteMeta(strings.ToLower(f.Phrase))
	if f.WholeWord {
		pat = `(^|[^\pL\pN_])` + pat + `($|[^\pL\pN_])`
	}
	re, err := regexp.Compile(pat)
	f.re = re
	return err == nil
}

// filterGroups returns a user's filter groups with their keywords, including
// expired ones.
func (app *App) filterGroups(accountID string) []*FilterGroup {
	rows, err := app.db.Query(`SELECT id, title, context, action, expires_at FROM filter_groups WHERE account_id=? ORDER BY created_at, id`, accountID)
	if err != nil {
		return nil
	}
	var out []*FilterGroup
	byID := map[string]*FilterGroup{}
	for rows.Next() {
		g := &FilterGroup{}
		var ctx string
		rows.Scan(&g.ID, &g.Title, &ctx, &g.Action, &g.ExpiresAt)
		if ctx != "" {
			g.Context = strings.Split(ctx, ",")
		}
		out = append(out, g)
		byID[g.ID] = g
	}
	rows.Close()
	rows, err = app.db.Query(`SELECT id, phrase, whole_word, group_id FROM filters WHERE account_id=? ORDER BY created_at, id`, accountID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		f := &Filter{}
		var gid string
		rows.Scan(&f.ID, &f.Phrase, &f.WholeWord, &gid)
		if g := byID[gid]; g != nil && f.compile() {
			f.Group, f.Action = g, g.Action
			g.Keywords = append(g.Keywords, f)
		}
	}
	return out
}

func (app *App) filterGroup(accountID, id string) *FilterGroup {
	for _, g := range app.filterGroups(accountID) {
		if g.ID == id {
			return g
		}
	}
	return nil
}

// filtersFor returns the keywords of a user's unexpired filters.
func (app *App) filtersFor(accountID string) []*Filter {
	var out []*Filter
	for _, g := range app.filterGroups(accountID) {
		if !g.expired() {
			out = append(out, g.Keywords...)
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

// applyFilters drops posts matching "hide" filters for the context (home,
// public, notifications, thread, account), marks posts matching "warn"
// filters so they are shown collapsed and hides the media of posts
// matching "blur" filters. Own posts are never filtered.
func (app *App) applyFilters(v *Account, sts []*Status, context string) []*Status {
	if v == nil {
		return sts
	}
	var filters []*Filter
	for _, f := range app.filtersFor(v.ID) {
		if contains(f.Group.Contexts(), context) {
			filters = append(filters, f)
		}
	}
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
					} else if f.Action == "blur" {
						o.Blurred = true
						if s.Filter == nil {
							s.Filter = f
						}
					} else if s.FilteredBy == "" {
						s.FilteredBy, s.Filter = f.Phrase, f
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

const maxFilterKeywords = 100

func checkFilterPhrase(phrase string) (string, error) {
	phrase = strings.TrimSpace(phrase)
	if phrase == "" || utf8.RuneCountInString(phrase) > 100 {
		return "", errors.New("filter phrases are 1-100 characters")
	}
	return phrase, nil
}

// SaveFilterGroup creates the group (empty ID) or updates it.
func (app *App) SaveFilterGroup(me *Account, g *FilterGroup) error {
	g.Title = truncateRunes(strings.TrimSpace(g.Title), 100)
	if g.Title == "" {
		return errors.New("a filter needs a title")
	}
	if g.Action != "hide" && g.Action != "blur" {
		g.Action = "warn"
	}
	var ctx []string
	for _, c := range g.Context {
		if contains(filterContexts, c) && !contains(ctx, c) {
			ctx = append(ctx, c)
		}
	}
	if len(ctx) == len(filterContexts) {
		ctx = nil
	}
	g.Context = ctx
	if g.ID == "" {
		if app.countInt(`SELECT COUNT(*) FROM filter_groups WHERE account_id=?`, me.ID) >= maxFilterKeywords {
			return errors.New("too many filters")
		}
		g.ID = ulid.New()
		_, err := app.db.Exec(`INSERT INTO filter_groups (id, account_id, title, context, action, expires_at, created_at) VALUES (?,?,?,?,?,?,?)`,
			g.ID, me.ID, g.Title, strings.Join(ctx, ","), g.Action, g.ExpiresAt, time.Now().Unix())
		return err
	}
	_, err := app.db.Exec(`UPDATE filter_groups SET title=?, context=?, action=?, expires_at=? WHERE id=? AND account_id=?`,
		g.Title, strings.Join(ctx, ","), g.Action, g.ExpiresAt, g.ID, me.ID)
	return err
}

// AddFilterKeyword adds a keyword to one of the user's groups.
func (app *App) AddFilterKeyword(me *Account, groupID, phrase string, wholeWord bool) (*Filter, error) {
	phrase, err := checkFilterPhrase(phrase)
	if err != nil {
		return nil, err
	}
	if app.countInt(`SELECT COUNT(*) FROM filters WHERE account_id=?`, me.ID) >= maxFilterKeywords {
		return nil, errors.New("too many filters")
	}
	f := &Filter{ID: ulid.New(), Phrase: phrase, WholeWord: wholeWord}
	_, err = app.db.Exec(`INSERT INTO filters (id, account_id, phrase, whole_word, created_at, group_id) VALUES (?,?,?,?,?,?)`,
		f.ID, me.ID, phrase, wholeWord, time.Now().Unix(), groupID)
	return f, err
}

func (app *App) UpdateFilterKeyword(me *Account, id, phrase string, wholeWord bool) error {
	phrase, err := checkFilterPhrase(phrase)
	if err != nil {
		return err
	}
	_, err = app.db.Exec(`UPDATE filters SET phrase=?, whole_word=? WHERE id=? AND account_id=?`, phrase, wholeWord, id, me.ID)
	return err
}

func (app *App) DeleteFilterKeyword(me *Account, id string) {
	app.db.Exec(`DELETE FROM filters WHERE id=? AND account_id=?`, id, me.ID)
}

func (app *App) DeleteFilterGroup(me *Account, id string) {
	app.db.Exec(`DELETE FROM filters WHERE group_id=? AND account_id=?`, id, me.ID)
	app.db.Exec(`DELETE FROM filter_groups WHERE id=? AND account_id=?`, id, me.ID)
}

// AddFilter is the web UI's one-phrase filter.
func (app *App) AddFilter(me *Account, phrase string, wholeWord bool, action string) error {
	phrase, err := checkFilterPhrase(phrase)
	if err != nil {
		return err
	}
	if app.countInt(`SELECT COUNT(*) FROM filters WHERE account_id=?`, me.ID) >= maxFilterKeywords {
		return errors.New("too many filters")
	}
	g := &FilterGroup{Title: phrase, Action: action}
	if err := app.SaveFilterGroup(me, g); err != nil {
		return err
	}
	_, err = app.AddFilterKeyword(me, g.ID, phrase, wholeWord)
	return err
}

// DeleteFilter removes a keyword (from the web UI), and its group once empty.
func (app *App) DeleteFilter(me *Account, keywordID string) {
	var gid string
	app.db.QueryRow(`SELECT group_id FROM filters WHERE id=? AND account_id=?`, keywordID, me.ID).Scan(&gid)
	app.DeleteFilterKeyword(me, keywordID)
	if gid != "" && app.countInt(`SELECT COUNT(*) FROM filters WHERE group_id=?`, gid) == 0 {
		app.DeleteFilterGroup(me, gid)
	}
}

// ---- reports ----

type Report struct {
	ID, Comment         string
	Reporter, Target    *Account
	Statuses            []*Status
	Forwarded, Remote   bool
	CreatedAt, Resolved time.Time
	Action, Note        string   // what the admin did (see reportActions) and their private note
	ResolvedBy          *Account // nil while open
	Earlier             int      // other reports about the same account
}

// FileReport stores a report by a local user and optionally forwards it to
// the target's server as a Flag from the instance actor (like Mastodon, so
// the reporter stays anonymous to the other server).
// FileReport stores a report for the admins and, if asked, forwards it to
// the target's server as a Flag. It returns the report's id.
func (app *App) FileReport(me, target *Account, statusIDs []string, comment string, forward bool) (string, error) {
	if me.ID == target.ID {
		return "", errors.New("you can't report yourself")
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
	id := ulid.New()
	comment = truncateRunes(strings.TrimSpace(comment), 1000)
	forwarded := forward && !target.IsLocal()
	if _, err := app.db.Exec(`INSERT INTO reports (id,uri,account_id,target_id,status_ids,comment,forwarded,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		id, app.cfg.BaseURL()+"/reports/"+id, me.ID, target.ID, ids, comment, forwarded, time.Now().Unix()); err != nil {
		return "", err
	}
	app.notifyAdmins(me.ID, "report")
	if forwarded {
		inst := app.instanceActor()
		objects := append([]any{target.URI}, toAny(uris)...)
		app.enqueue(inst, []string{target.Inbox}, withContext(obj{
			"id": app.cfg.BaseURL() + "/reports/" + id, "type": "Flag", "actor": inst.URI, "content": comment, "object": objects,
		}))
	}
	return id, nil
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
			ulid.New(), a, typ, fromID, time.Now().Unix())
	}
}

// inFlag stores reports other servers send about our users.
func (app *App) inFlag(actor *Account, act obj) error {
	var target *Account
	var statusIDs []string
	for _, iri := range ap.IDs(act["object"]) {
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
		ulid.New(), ap.Str(act, "id"), actor.ID, target.ID, ids, truncateRunes(plainText(ap.Str(act, "content")), 1000), time.Now().Unix())
	if err == nil {
		app.notifyAdmins(actor.ID, "report")
	}
	return err
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

// Private notes about other accounts (Mastodon's "note" on a relationship),
// only ever shown to their author.
func (app *App) SetAccountNote(me, target *Account, comment string) error {
	comment = strings.TrimSpace(comment)
	if utf8.RuneCountInString(comment) > 2000 {
		return errors.New("the note is too long (2000 characters at most)")
	}
	if comment == "" {
		_, err := app.db.Exec(`DELETE FROM account_notes WHERE account_id=? AND target_id=?`, me.ID, target.ID)
		return err
	}
	_, err := app.db.Exec(`INSERT INTO account_notes (account_id, target_id, comment) VALUES (?,?,?)
		ON CONFLICT(account_id, target_id) DO UPDATE SET comment=excluded.comment`, me.ID, target.ID, comment)
	return err
}

func (app *App) accountNote(me, target *Account) string {
	var c string
	app.db.QueryRow(`SELECT comment FROM account_notes WHERE account_id=? AND target_id=?`, me.ID, target.ID).Scan(&c)
	return c
}
