package server

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Conversations for the Mastodon API: direct-message threads, keyed by the
// thread's first direct post. Read and hidden state per user is kept in
// conversation_marks as the newest status id read or hidden; a newer
// message makes a hidden conversation show up again.

const maxConversationStatuses = 2000 // newest direct messages considered

type conversation struct {
	RootID       string
	Last         *Status
	Participants []*Account // without the viewer
	Unread       bool
	Filtered     bool // the last message waits as a filtered notification (a stranger's DM)
}

func (app *App) conversations(me *Account) []*conversation {
	sts, _ := app.queryStatusesNoQuotes(me, `s.visibility='direct' AND (s.account_id=? OR EXISTS(SELECT 1 FROM mentions m WHERE m.status_id=s.id AND m.account_id=?))`,
		[]any{me.ID, me.ID}, "", maxConversationStatuses, false)
	byID := map[string]*Status{}
	for _, s := range sts {
		byID[s.ID] = s
	}
	root := func(s *Status) string {
		seen := map[string]bool{}
		for s.InReplyToID != "" && byID[s.InReplyToID] != nil && !seen[s.ID] {
			seen[s.ID] = true
			s = byID[s.InReplyToID]
		}
		return s.ID
	}
	marks := map[string][2]string{}
	if rows, err := app.db.Query(`SELECT root_id, read_id, hidden_id FROM conversation_marks WHERE account_id=?`, me.ID); err == nil {
		for rows.Next() {
			var id, read, hidden string
			rows.Scan(&id, &read, &hidden)
			marks[id] = [2]string{read, hidden}
		}
		rows.Close()
	}
	var out []*conversation
	byRoot := map[string]*conversation{}
	seen := map[string]map[string]bool{}
	for _, s := range sts { // newest first
		r := root(s)
		c := byRoot[r]
		if c == nil {
			c = &conversation{RootID: r, Last: s}
			byRoot[r], seen[r] = c, map[string]bool{me.ID: true}
			out = append(out, c)
		}
		for _, a := range append([]*Account{s.Account}, s.Mentions...) {
			if a != nil && !seen[r][a.ID] {
				seen[r][a.ID] = true
				c.Participants = append(c.Participants, a)
			}
		}
	}
	filtered := map[string]bool{}
	if rows, err := app.db.Query(`SELECT status_id FROM notifications WHERE account_id=? AND filtered=1 AND type='mention'`, me.ID); err == nil {
		for rows.Next() {
			var id string
			rows.Scan(&id)
			filtered[id] = true
		}
		rows.Close()
	}
	shown := out[:0]
	for _, c := range out {
		c.Filtered = filtered[c.Last.ID]
		m := marks[c.RootID]
		if c.Last.ID <= m[1] {
			continue
		}
		c.Unread = c.Last.AccountID != me.ID && c.Last.ID > m[0]
		shown = append(shown, c)
	}
	sort.SliceStable(shown, func(i, j int) bool { return shown[i].Last.ID > shown[j].Last.ID })
	return shown
}

// The sidebar badge counts unread conversations on every page view, so the
// count is cached per account until a direct message arrives or is deleted,
// a conversation is marked, or a minute passes (mutes and blocks change it too).
var (
	dmUnreadMu    sync.Mutex
	dmUnreadCache = map[string]dmUnread{}
)

type dmUnread struct {
	key string
	at  time.Time
	n   int
}

// forgetUnreadDMs drops the cached count when what it depends on changed.
func forgetUnreadDMs(accountID string) {
	dmUnreadMu.Lock()
	delete(dmUnreadCache, accountID)
	dmUnreadMu.Unlock()
}

func (app *App) unreadConversations(me *Account) int {
	var count int
	var last string
	app.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(id),'') FROM statuses WHERE visibility='direct'
		AND (account_id=? OR id IN (SELECT status_id FROM mentions WHERE account_id=?))`, me.ID, me.ID).Scan(&count, &last)
	if count == 0 {
		return 0
	}
	key := last + "/" + strconv.Itoa(count)
	dmUnreadMu.Lock()
	c, ok := dmUnreadCache[me.ID]
	dmUnreadMu.Unlock()
	if ok && c.key == key && time.Since(c.at) < time.Minute {
		return c.n
	}
	n := 0
	for _, cv := range app.conversations(me) {
		if cv.Unread && !cv.Filtered {
			n++
		}
	}
	dmUnreadMu.Lock()
	dmUnreadCache[me.ID] = dmUnread{key, time.Now(), n}
	dmUnreadMu.Unlock()
	return n
}

func (app *App) apiConversation(me *Account, c *conversation) obj {
	accs := []any{}
	for _, a := range c.Participants {
		accs = append(accs, app.apiAccount(a))
	}
	sts := []*Status{c.Last}
	app.hydrateQuotes(me, sts)
	return obj{"id": c.RootID, "unread": c.Unread, "accounts": accs, "last_status": app.apiStatus(me, sts[0])}
}

// apiConversations pages by the id of each conversation's last status, like Mastodon.
func (app *App) apiConversations(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	pq := pageParams(r, 20, 40)
	all := app.conversations(c.Me)
	var page []*conversation
	if pq.MinID != "" {
		for i := len(all) - 1; i >= 0 && len(page) < pq.Limit; i-- {
			if all[i].Last.ID > pq.MinID {
				page = append([]*conversation{all[i]}, page...)
			}
		}
	} else {
		for _, cv := range all {
			if (pq.MaxID == "" || cv.Last.ID < pq.MaxID) && (pq.SinceID == "" || cv.Last.ID > pq.SinceID) && len(page) < pq.Limit {
				page = append(page, cv)
			}
		}
	}
	out := []any{}
	for _, cv := range page {
		out = append(out, app.apiConversation(c.Me, cv))
	}
	if len(page) > 0 {
		app.setLink(w, r, page[0].Last.ID, page[len(page)-1].Last.ID, len(page) == pq.Limit)
	}
	apiJSON(w, 200, out)
}

func (app *App) findConversation(me *Account, id string) *conversation {
	for _, c := range app.conversations(me) {
		if c.RootID == id {
			return c
		}
	}
	return nil
}

func (app *App) markConversation(me *Account, c *conversation, column string) {
	app.db.Exec(`INSERT INTO conversation_marks (account_id, root_id, `+column+`) VALUES (?,?,?)
		ON CONFLICT (account_id, root_id) DO UPDATE SET `+column+`=excluded.`+column, me.ID, c.RootID, c.Last.ID)
	forgetUnreadDMs(me.ID)
}

// POST /api/v1/conversations/:id/read
func (app *App) apiConversationRead(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	cv := app.findConversation(c.Me, r.PathValue("id"))
	if cv == nil {
		apiError(w, 404, "Record not found")
		return
	}
	app.markConversation(c.Me, cv, "read_id")
	cv.Unread = false
	apiJSON(w, 200, app.apiConversation(c.Me, cv))
}

// DELETE /api/v1/conversations/:id hides it until a new message arrives.
func (app *App) apiConversationDelete(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	cv := app.findConversation(c.Me, r.PathValue("id"))
	if cv == nil {
		apiError(w, 404, "Record not found")
		return
	}
	app.markConversation(c.Me, cv, "hidden_id")
	apiJSON(w, 200, obj{})
}

// ---- web UI: the Direct messages page ----

func (app *App) handleConversations(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section, p.Heading = "Direct messages", "conversations", "Direct messages"
	var all []*conversation
	for _, cv := range app.conversations(p.Me) {
		if cv.Filtered { // they wait under Notifications → Filtered
			p.FilteredDMs++
		} else if len(all) < 50 {
			all = append(all, cv)
		}
	}
	p.Conversations = all
	p.Compose = &Compose{Visibility: "direct"}
	app.render(w, "conversations", p)
}

// handleConversationOpen marks a conversation read and shows its newest message in its thread.
func (app *App) handleConversationOpen(w http.ResponseWriter, r *http.Request, p *Page) {
	cv := app.findConversation(p.Me, r.PathValue("id"))
	if cv == nil {
		app.notFound(w, p)
		return
	}
	app.markConversation(p.Me, cv, "read_id")
	http.Redirect(w, r, cv.Last.Path()+"#s-"+cv.Last.ID, http.StatusSeeOther)
}

// handleConversationHide hides a conversation until a new message arrives.
func (app *App) handleConversationHide(w http.ResponseWriter, r *http.Request, p *Page) {
	if cv := app.findConversation(p.Me, r.PathValue("id")); cv != nil {
		app.markConversation(p.Me, cv, "hidden_id")
	}
	http.Redirect(w, r, "/conversations", http.StatusSeeOther)
}
