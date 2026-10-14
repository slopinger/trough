package main

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
