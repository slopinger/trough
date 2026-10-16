package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"git.freifick.net/slop/trough/internal/ap"
)

// Deleting your own account (Settings → Delete account). Posts, media,
// follows, notifications and everything else of the account go away; other
// servers get a Delete of the actor. The account row stays behind as a
// suspended tombstone: it keeps the signing key for that Delete, keeps the
// username from being taken by someone else (other servers remember the
// actor), and makes the actor answer 410 Gone.

func (app *App) DeleteOwnAccount(me *Account) error {
	if !me.IsLocal() {
		return errors.New("not a local account")
	}
	inboxes := uniq(append(app.followerInboxes(me.ID), app.followingInboxes(me.ID)...))
	act := obj{"@context": ap.Context, "id": me.URI + "#delete", "type": "Delete", "actor": me.URI, "object": me.URI,
		"to": []any{ap.Public}}

	rows, err := app.db.Query(`SELECT id FROM statuses WHERE account_id=?`, me.ID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		app.purgeStatus(id)
	}
	app.removeMediaFiles(`account_id=?`, me.ID)
	for _, u := range []string{me.Avatar, me.Header} {
		if strings.Contains(u, "/media/") {
			os.Remove(filepath.Join(app.mediaDir, filepath.Base(u)))
		}
	}
	if err := app.deleteAccountRows(me.ID); err != nil {
		return err
	}
	if _, err := app.db.Exec(`UPDATE accounts SET display_name='', note='', note_source='', avatar='', header='', fields='[]', emojis='[]',
		also_known_as='[]', moved_to='', attribution_domains='[]', discoverable=0, locked=0, suspended=1 WHERE id=?`, me.ID); err != nil {
		return err
	}
	app.enqueue(me, inboxes, act)
	log.Printf("account @%s deleted by its owner; telling %d inboxes", me.Username, len(inboxes))
	return nil
}

// deleteAccountRows removes every row that references the account through a
// foreign key, except the account itself (the user row included).
func (app *App) deleteAccountRows(accountID string) error {
	tables, err := app.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name != 'accounts'`)
	if err != nil {
		return err
	}
	var names []string
	for tables.Next() {
		var n string
		tables.Scan(&n)
		names = append(names, n)
	}
	tables.Close()
	for _, t := range names {
		fks, err := app.db.Query(fmt.Sprintf(`SELECT "from" FROM pragma_foreign_key_list('%s') WHERE "table"='accounts'`, t))
		if err != nil {
			return err
		}
		var cols []string
		for fks.Next() {
			var c string
			fks.Scan(&c)
			cols = append(cols, c)
		}
		fks.Close()
		for _, c := range cols {
			if _, err := app.db.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE "%s"=?`, t, c), accountID); err != nil {
				return fmt.Errorf("%s.%s: %w", t, c, err)
			}
		}
	}
	return nil
}

func (app *App) followingInboxes(accountID string) []string {
	rows, err := app.db.Query(`SELECT DISTINCT CASE WHEN a.shared_inbox != '' THEN a.shared_inbox ELSE a.inbox END
		FROM follows f JOIN accounts a ON a.id=f.target_id WHERE f.account_id=? AND a.domain != '' AND a.suspended=0`, accountID)
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

// deletedLocalActor answers for a local account deleted by its owner (410 Gone).
func (app *App) deletedLocalActor(w http.ResponseWriter, username string) bool {
	acc, err := app.accountWhere(`a.domain='' AND a.username=? COLLATE NOCASE AND a.suspended=1`, username)
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", "application/activity+json")
	w.WriteHeader(http.StatusGone)
	json.NewEncoder(w).Encode(obj{"@context": ap.Context, "id": acc.URI, "type": "Tombstone"})
	return true
}

func (app *App) handleDeleteAccount(w http.ResponseWriter, r *http.Request, p *Page) {
	fail := func(msg string) {
		setFlash(w, msg)
		http.Redirect(w, r, "/settings#delete", http.StatusSeeOther)
	}
	if !strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(r.FormValue("confirm")), "@"), p.Me.Username) {
		fail("Type your username to confirm.")
		return
	}
	if _, err := app.checkPassword(p.Me.Username, r.FormValue("password")); err != nil {
		fail("Wrong password; your account was not deleted.")
		return
	}
	if app.twoFactorEnabled(p.Me.ID) && !app.checkSecondFactor(p.Me.ID, r.FormValue("code")) {
		fail("Wrong two-factor code; your account was not deleted.")
		return
	}
	if app.isAdmin(p.Me.ID) && app.countInt(`SELECT COUNT(*) FROM users WHERE admin=1`) == 1 {
		fail("You are the only admin. Make someone else an admin first.")
		return
	}
	if err := app.DeleteOwnAccount(p.Me); err != nil {
		fail("Could not delete the account: " + err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	setFlash(w, "Your account has been deleted. Goodbye!")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
