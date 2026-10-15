package server

import "time"

// Muting a conversation: no more notifications about replies to, likes of
// or boosts of posts in that thread, as Mastodon's status mute. A thread is
// identified by its first post as far as this server knows it.

// threadRoot is the first post of statusID's thread (following replies up).
func (app *App) threadRoot(statusID string) string {
	var root string
	app.db.QueryRow(`WITH RECURSIVE up(id, parent, depth) AS (
			SELECT id, in_reply_to_id, 0 FROM statuses WHERE id=?
			UNION ALL SELECT s.id, s.in_reply_to_id, up.depth+1 FROM statuses s JOIN up ON s.id=up.parent
			WHERE up.parent != '' AND up.depth < 200)
		SELECT id FROM up ORDER BY depth DESC LIMIT 1`, statusID).Scan(&root)
	return root
}

func (app *App) MuteConversation(me *Account, st *Status) error {
	root := app.threadRoot(st.ID)
	if root == "" {
		return errNotFound
	}
	_, err := app.db.Exec(`INSERT OR IGNORE INTO conversation_mutes (account_id, root_id, created_at) VALUES (?,?,?)`, me.ID, root, time.Now().Unix())
	return err
}

func (app *App) UnmuteConversation(me *Account, st *Status) error {
	_, err := app.db.Exec(`DELETE FROM conversation_mutes WHERE account_id=? AND root_id=?`, me.ID, app.threadRoot(st.ID))
	return err
}

// conversationMuted reports whether me muted the thread statusID is in.
func (app *App) conversationMuted(me *Account, statusID string) bool {
	if me == nil || statusID == "" {
		return false
	}
	var any bool
	if app.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM conversation_mutes WHERE account_id=?)`, me.ID).Scan(&any); !any {
		return false
	}
	return app.db.QueryRow(`SELECT 1 FROM conversation_mutes WHERE account_id=? AND root_id=?`, me.ID, app.threadRoot(statusID)).Scan(new(int)) == nil
}
