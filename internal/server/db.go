package server

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// schema is applied incrementally: migrations[i] brings the database from
// version i to i+1 (tracked in PRAGMA user_version).
var migrations = []string{`
CREATE TABLE accounts (
	id TEXT PRIMARY KEY,
	username TEXT NOT NULL,
	domain TEXT NOT NULL DEFAULT '',
	uri TEXT NOT NULL UNIQUE,
	url TEXT NOT NULL DEFAULT '',
	display_name TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT '',
	note_source TEXT NOT NULL DEFAULT '',
	avatar TEXT NOT NULL DEFAULT '',
	header TEXT NOT NULL DEFAULT '',
	inbox TEXT NOT NULL DEFAULT '',
	shared_inbox TEXT NOT NULL DEFAULT '',
	outbox TEXT NOT NULL DEFAULT '',
	followers_uri TEXT NOT NULL DEFAULT '',
	following_uri TEXT NOT NULL DEFAULT '',
	public_key_id TEXT NOT NULL DEFAULT '',
	public_key TEXT NOT NULL DEFAULT '',
	private_key TEXT NOT NULL DEFAULT '',
	actor_type TEXT NOT NULL DEFAULT 'Person',
	locked INTEGER NOT NULL DEFAULT 0,
	fields TEXT NOT NULL DEFAULT '[]',
	emojis TEXT NOT NULL DEFAULT '[]',
	created_at INTEGER NOT NULL,
	fetched_at INTEGER NOT NULL DEFAULT 0,
	suspended INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX accounts_local_username ON accounts(username COLLATE NOCASE) WHERE domain = '';
CREATE INDEX accounts_acct ON accounts(username COLLATE NOCASE, domain);
CREATE INDEX accounts_key ON accounts(public_key_id);
CREATE INDEX accounts_domain ON accounts(domain);

CREATE TABLE users (
	account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
	password_hash TEXT NOT NULL,
	admin INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	last_seen INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE statuses (
	id TEXT PRIMARY KEY,
	uri TEXT NOT NULL UNIQUE,
	url TEXT NOT NULL DEFAULT '',
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	local INTEGER NOT NULL DEFAULT 0,
	in_reply_to_uri TEXT NOT NULL DEFAULT '',
	in_reply_to_id TEXT NOT NULL DEFAULT '',
	in_reply_to_account_id TEXT NOT NULL DEFAULT '',
	boost_of_id TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT '',
	spoiler TEXT NOT NULL DEFAULT '',
	sensitive INTEGER NOT NULL DEFAULT 0,
	visibility TEXT NOT NULL,
	language TEXT NOT NULL DEFAULT '',
	emojis TEXT NOT NULL DEFAULT '[]',
	poll TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	edited_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX statuses_account ON statuses(account_id, id);
CREATE INDEX statuses_reply_id ON statuses(in_reply_to_id);
CREATE INDEX statuses_reply_uri ON statuses(in_reply_to_uri);
CREATE INDEX statuses_boost ON statuses(boost_of_id);
CREATE INDEX statuses_vis ON statuses(visibility, id);

CREATE TABLE mentions (
	status_id TEXT NOT NULL REFERENCES statuses(id) ON DELETE CASCADE,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	PRIMARY KEY (status_id, account_id)
);
CREATE INDEX mentions_account ON mentions(account_id, status_id);

CREATE TABLE status_tags (
	status_id TEXT NOT NULL REFERENCES statuses(id) ON DELETE CASCADE,
	tag TEXT NOT NULL,
	PRIMARY KEY (status_id, tag)
);
CREATE INDEX status_tags_tag ON status_tags(tag, status_id);

CREATE TABLE attachments (
	id TEXT PRIMARY KEY,
	status_id TEXT REFERENCES statuses(id) ON DELETE CASCADE,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	url TEXT NOT NULL,
	media_type TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	file TEXT NOT NULL DEFAULT '',
	width INTEGER NOT NULL DEFAULT 0,
	height INTEGER NOT NULL DEFAULT 0,
	position INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX attachments_status ON attachments(status_id, position);

CREATE TABLE follows (
	id TEXT PRIMARY KEY,
	uri TEXT NOT NULL UNIQUE,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	target_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	accepted INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	UNIQUE (account_id, target_id)
);
CREATE INDEX follows_target ON follows(target_id, accepted);

CREATE TABLE likes (
	id TEXT PRIMARY KEY,
	uri TEXT NOT NULL UNIQUE,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	status_id TEXT NOT NULL REFERENCES statuses(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	UNIQUE (account_id, status_id)
);
CREATE INDEX likes_status ON likes(status_id);

CREATE TABLE blocks (
	id TEXT PRIMARY KEY,
	uri TEXT NOT NULL UNIQUE,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	target_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	UNIQUE (account_id, target_id)
);
CREATE INDEX blocks_target ON blocks(target_id);

CREATE TABLE notifications (
	id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	type TEXT NOT NULL,
	from_account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	status_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	read INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX notifications_account ON notifications(account_id, id);

CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	csrf TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	user_agent TEXT NOT NULL DEFAULT ''
);

CREATE TABLE deliveries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	account_id TEXT NOT NULL,
	inbox TEXT NOT NULL,
	payload TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_try INTEGER NOT NULL,
	last_error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX deliveries_next ON deliveries(next_try);

CREATE TABLE domain_blocks (
	domain TEXT PRIMARY KEY,
	reason TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);

CREATE TABLE settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`, `
ALTER TABLE statuses ADD COLUMN pinned_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE statuses ADD COLUMN quote_uri TEXT NOT NULL DEFAULT '';
ALTER TABLE statuses ADD COLUMN card_url TEXT NOT NULL DEFAULT '';
CREATE INDEX statuses_pinned ON statuses(account_id, pinned_at) WHERE pinned_at > 0;
ALTER TABLE accounts ADD COLUMN featured_uri TEXT NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN also_known_as TEXT NOT NULL DEFAULT '[]';
ALTER TABLE accounts ADD COLUMN moved_to TEXT NOT NULL DEFAULT '';

CREATE TABLE bookmarks (
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	status_id TEXT NOT NULL REFERENCES statuses(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (account_id, status_id)
);

CREATE TABLE mutes (
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	target_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (account_id, target_id)
);

CREATE TABLE filters (
	id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	phrase TEXT NOT NULL,
	whole_word INTEGER NOT NULL DEFAULT 1,
	action TEXT NOT NULL DEFAULT 'warn',
	created_at INTEGER NOT NULL
);
CREATE INDEX filters_account ON filters(account_id);

CREATE TABLE poll_votes (
	status_id TEXT NOT NULL REFERENCES statuses(id) ON DELETE CASCADE,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	choice INTEGER NOT NULL,
	uri TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (status_id, account_id, choice)
);

CREATE TABLE reactions (
	status_id TEXT NOT NULL REFERENCES statuses(id) ON DELETE CASCADE,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	emoji TEXT NOT NULL,
	emoji_url TEXT NOT NULL DEFAULT '',
	uri TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (status_id, account_id, emoji)
);
CREATE INDEX reactions_uri ON reactions(uri);

CREATE TABLE reports (
	id TEXT PRIMARY KEY,
	uri TEXT NOT NULL DEFAULT '',
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	target_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	status_ids TEXT NOT NULL DEFAULT '[]',
	comment TEXT NOT NULL DEFAULT '',
	forwarded INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	resolved_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE invites (
	code TEXT PRIMARY KEY,
	created_by TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	uses INTEGER NOT NULL DEFAULT 0,
	max_uses INTEGER NOT NULL DEFAULT 1,
	expires_at INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);

CREATE TABLE link_cards (
	url TEXT PRIMARY KEY,
	title TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	image TEXT NOT NULL DEFAULT '',
	site TEXT NOT NULL DEFAULT '',
	fetched_at INTEGER NOT NULL
);
`, `
CREATE TABLE oauth_apps (
	id TEXT PRIMARY KEY,
	client_id TEXT NOT NULL UNIQUE,
	client_secret TEXT NOT NULL,
	name TEXT NOT NULL,
	website TEXT NOT NULL DEFAULT '',
	redirect_uris TEXT NOT NULL,
	scopes TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE oauth_codes (
	code TEXT PRIMARY KEY,
	app_id TEXT NOT NULL REFERENCES oauth_apps(id) ON DELETE CASCADE,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	redirect_uri TEXT NOT NULL,
	scopes TEXT NOT NULL,
	challenge TEXT NOT NULL DEFAULT '',
	challenge_method TEXT NOT NULL DEFAULT '',
	expires_at INTEGER NOT NULL
);

CREATE TABLE oauth_tokens (
	id TEXT PRIMARY KEY,
	token_hash TEXT NOT NULL UNIQUE,
	app_id TEXT NOT NULL REFERENCES oauth_apps(id) ON DELETE CASCADE,
	account_id TEXT REFERENCES accounts(id) ON DELETE CASCADE,
	scopes TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	last_used INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE markers (
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	timeline TEXT NOT NULL,
	last_read_id TEXT NOT NULL,
	version INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (account_id, timeline)
);
`, `
CREATE TABLE relays (
	id TEXT PRIMARY KEY,
	inbox TEXT NOT NULL UNIQUE,
	actor_uri TEXT NOT NULL DEFAULT '',
	follow_uri TEXT NOT NULL,
	style TEXT NOT NULL DEFAULT 'mastodon',
	accepted INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);

CREATE TABLE followed_tags (
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	tag TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (account_id, tag)
);

CREATE TABLE lists (
	id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	title TEXT NOT NULL,
	replies_policy TEXT NOT NULL DEFAULT 'list',
	exclusive INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX lists_account ON lists(account_id);

CREATE TABLE list_accounts (
	list_id TEXT NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	PRIMARY KEY (list_id, account_id)
);
`, `
CREATE TABLE status_edits (
	id TEXT PRIMARY KEY,
	status_id TEXT NOT NULL REFERENCES statuses(id) ON DELETE CASCADE,
	content TEXT NOT NULL,
	spoiler TEXT NOT NULL DEFAULT '',
	sensitive INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX status_edits_status ON status_edits(status_id, created_at);
`, `
CREATE TABLE custom_emojis (
	shortcode TEXT PRIMARY KEY,
	file TEXT NOT NULL,
	url TEXT NOT NULL,
	media_type TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
`, `
CREATE TABLE push_subscriptions (
	id TEXT PRIMARY KEY,
	token_id TEXT NOT NULL UNIQUE REFERENCES oauth_tokens(id) ON DELETE CASCADE,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	endpoint TEXT NOT NULL,
	p256dh TEXT NOT NULL,
	auth TEXT NOT NULL,
	access_token TEXT NOT NULL,
	alerts TEXT NOT NULL DEFAULT '{}',
	policy TEXT NOT NULL DEFAULT 'all',
	created_at INTEGER NOT NULL
);
CREATE INDEX push_subscriptions_account ON push_subscriptions(account_id);
`, `
CREATE TABLE scheduled_statuses (
	id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	params TEXT NOT NULL,
	media_ids TEXT NOT NULL DEFAULT '[]',
	scheduled_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX scheduled_due ON scheduled_statuses(scheduled_at);
`, `
CREATE TABLE signature_styles (
	host TEXT PRIMARY KEY,
	style TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
`, `
-- several relays can share one inbox (fedi.buzz tag relays)
CREATE TABLE relays_new (
	id TEXT PRIMARY KEY,
	inbox TEXT NOT NULL,
	actor_uri TEXT NOT NULL DEFAULT '',
	follow_uri TEXT NOT NULL,
	style TEXT NOT NULL DEFAULT 'mastodon',
	accepted INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	UNIQUE (inbox, actor_uri)
);
INSERT INTO relays_new SELECT id, inbox, actor_uri, follow_uri, style, accepted, created_at FROM relays;
DROP TABLE relays;
ALTER TABLE relays_new RENAME TO relays;
`, `
CREATE TABLE account_notes (
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	target_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	comment TEXT NOT NULL,
	PRIMARY KEY (account_id, target_id)
);
`, `
CREATE TABLE conversation_marks (
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	root_id TEXT NOT NULL,
	read_id TEXT NOT NULL DEFAULT '',
	hidden_id TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (account_id, root_id)
);
`, `
CREATE TABLE filter_groups (
	id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	title TEXT NOT NULL,
	context TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL DEFAULT 'warn',
	expires_at INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX filter_groups_account ON filter_groups(account_id);
ALTER TABLE filters ADD COLUMN group_id TEXT NOT NULL DEFAULT '';
INSERT INTO filter_groups (id, account_id, title, action, created_at) SELECT id, account_id, phrase, action, created_at FROM filters;
UPDATE filters SET group_id=id;
CREATE INDEX filters_group ON filters(group_id);
`, `
ALTER TABLE statuses ADD COLUMN quote_state TEXT NOT NULL DEFAULT '';
ALTER TABLE statuses ADD COLUMN quote_auth TEXT NOT NULL DEFAULT '';
ALTER TABLE statuses ADD COLUMN quote_policy TEXT NOT NULL DEFAULT '';
CREATE INDEX statuses_quote_uri ON statuses(quote_uri) WHERE quote_uri != '';
CREATE TABLE quote_authorizations (
	id TEXT PRIMARY KEY,
	status_id TEXT NOT NULL REFERENCES statuses(id) ON DELETE CASCADE,
	quoting_uri TEXT NOT NULL,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	UNIQUE (status_id, quoting_uri)
);
`, `
CREATE TABLE account_pins (
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	target_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (account_id, target_id)
);
`, `
ALTER TABLE accounts ADD COLUMN attribution_domains TEXT NOT NULL DEFAULT '[]';
`, `
ALTER TABLE users ADD COLUMN quote_policy TEXT NOT NULL DEFAULT 'public';
`}

func openDB(dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "trough.sqlite3")
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return nil, err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return db, nil
}

func (a *App) getSetting(key string) string {
	var v string
	a.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

func (a *App) setSetting(key, value string) error {
	_, err := a.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
