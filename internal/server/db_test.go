package server

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// Filters from before filter groups each become a group of their own.
func TestFilterGroupMigration(t *testing.T) {
	dir := t.TempDir()
	old, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "trough.sqlite3")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const before = 12 // the last migration before filter_groups
	for i := 0; i < before; i++ {
		if _, err := old.Exec(migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	old.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, before))
	if _, err := old.Exec(`INSERT INTO accounts (id, username, domain, uri, created_at) VALUES ('a1', 'bob', '', 'https://x/users/bob', 0)`); err != nil {
		t.Fatal(err)
	}
	old.Exec(`INSERT INTO filters (id, account_id, phrase, whole_word, action, created_at) VALUES ('f1', 'a1', 'politics', 1, 'hide', 5)`)
	old.Close()

	db, err := openDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app := &App{db: db}
	gs := app.filterGroups("a1")
	if len(gs) != 1 || gs[0].ID != "f1" || gs[0].Title != "politics" || gs[0].Action != "hide" || len(gs[0].Keywords) != 1 ||
		gs[0].Keywords[0].Phrase != "politics" || len(gs[0].Contexts()) != len(filterContexts) {
		t.Fatalf("migrated groups = %+v", gs)
	}
}
