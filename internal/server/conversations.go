package server

import (
	"net/http"
	"sort"
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
	shown := out[:0]
	for _, c := range out {
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
