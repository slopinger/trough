package server

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CSV export and import in Mastodon's formats (Settings → Import and
// export), so follows, blocks, mutes, blocked servers and bookmarks move
// between trough and Mastodon-compatible servers. Imports merge into what
// is there and run in the background, since remote accounts and posts
// have to be looked up first.

const maxImportRows = 2000

var exportFiles = map[string]string{
	"following_accounts.csv": "following",
	"blocked_accounts.csv":   "blocks",
	"muted_accounts.csv":     "mutes",
	"blocked_domains.csv":    "domain_blocks",
	"bookmarks.csv":          "bookmarks",
	"lists.csv":              "lists",
}

// fullAcct is user@domain, also for local accounts.
func (app *App) fullAcct(a *Account) string {
	if a.IsLocal() {
		return a.Username + "@" + app.cfg.Host
	}
	return a.Acct()
}

func (app *App) exportCSV(me *Account, kind string, w io.Writer) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	accounts := func(q string) ([]*Account, error) {
		return app.accountsQuery(`SELECT `+accountCols+` FROM `+q+` ORDER BY x.created_at`, me.ID)
	}
	switch kind {
	case "following":
		accs, err := accounts(`follows x JOIN accounts a ON a.id=x.target_id WHERE x.account_id=? AND x.accepted=1`)
		if err != nil {
			return err
		}
		cw.Write([]string{"Account address", "Show boosts", "Notify on new posts", "Languages"})
		for _, a := range accs {
			r := app.relationship(me.ID, a.ID)
			cw.Write([]string{app.fullAcct(a), strconv.FormatBool(r.ShowingReblogs), strconv.FormatBool(r.Notifying), ""})
		}
	case "blocks", "mutes":
		accs, err := accounts(kind + ` x JOIN accounts a ON a.id=x.target_id WHERE x.account_id=?`)
		if err != nil {
			return err
		}
		if kind == "mutes" {
			cw.Write([]string{"Account address", "Hide notifications"})
		}
		for _, a := range accs {
			if kind == "mutes" {
				cw.Write([]string{app.fullAcct(a), strconv.FormatBool(app.relationship(me.ID, a.ID).MutingNotifications)})
			} else {
				cw.Write([]string{app.fullAcct(a)})
			}
		}
	case "domain_blocks":
		for _, d := range app.userDomainBlocks(me) {
			cw.Write([]string{d})
		}
	case "bookmarks":
		rows, err := app.db.Query(`SELECT s.uri FROM bookmarks b JOIN statuses s ON s.id=b.status_id WHERE b.account_id=? ORDER BY b.created_at`, me.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var uri string
			rows.Scan(&uri)
			cw.Write([]string{uri})
		}
	case "lists":
		for _, l := range app.lists(me) {
			accs, _ := app.accountsQuery(`SELECT `+accountCols+` FROM list_accounts x JOIN accounts a ON a.id=x.account_id WHERE x.list_id=?`, l.ID)
			for _, a := range accs {
				cw.Write([]string{l.Title, app.fullAcct(a)})
			}
		}
	default:
		return errNotFound
	}
	return nil
}

// importState is the progress of a user's running or last import.
type importState struct {
	Kind              string
	Total, Done, Fail int
	Running           bool
	Finished          time.Time
}

var (
	importMu sync.Mutex
	imports  = map[string]*importState{} // by account ID
)

func importStatus(accountID string) *importState {
	importMu.Lock()
	defer importMu.Unlock()
	if s, ok := imports[accountID]; ok {
		c := *s
		return &c
	}
	return nil
}

// readImportRows returns the first column of each row (two columns for
// lists), skipping Mastodon's header line.
func readImportRows(r io.Reader, kind string) ([][]string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	var out [][]string
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("not a CSV file: %w", err)
		}
		if len(rec) == 0 || strings.TrimSpace(rec[0]) == "" || strings.EqualFold(strings.TrimSpace(rec[0]), "Account address") {
			continue
		}
		if kind == "lists" && len(rec) < 2 {
			continue
		}
		out = append(out, rec)
		if len(out) > maxImportRows {
			return nil, fmt.Errorf("at most %d rows can be imported at once", maxImportRows)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the file has no entries")
	}
	return out, nil
}

// startImport checks the file and imports it in the background.
func (app *App) startImport(me *Account, kind string, r io.Reader) (int, error) {
	switch kind {
	case "following", "blocks", "mutes", "domain_blocks", "bookmarks", "lists":
	default:
		return 0, errors.New("unknown import type")
	}
	rows, err := readImportRows(r, kind)
	if err != nil {
		return 0, err
	}
	importMu.Lock()
	if s, ok := imports[me.ID]; ok && s.Running {
		importMu.Unlock()
		return 0, errors.New("an import is still running")
	}
	st := &importState{Kind: kind, Total: len(rows), Running: true}
	imports[me.ID] = st
	importMu.Unlock()
	go func() {
		lists := map[string]*List{}
		for _, l := range app.lists(me) {
			lists[l.Title] = &l
		}
		for _, rec := range rows {
			err := app.importRow(me, kind, rec, lists)
			importMu.Lock()
			st.Done++
			if err != nil {
				st.Fail++
			}
			importMu.Unlock()
		}
		importMu.Lock()
		st.Running, st.Finished = false, time.Now()
		importMu.Unlock()
		log.Printf("import %s for @%s: %d entries, %d failed", kind, me.Username, st.Total, st.Fail)
	}()
	return len(rows), nil
}

func (app *App) importRow(me *Account, kind string, rec []string, lists map[string]*List) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	val := strings.TrimSpace(rec[0])
	switch kind {
	case "domain_blocks":
		return app.BlockDomain(me, val)
	case "bookmarks":
		st, err := app.resolveStatus(ctx, val)
		if err != nil {
			return err
		}
		return app.Bookmark(me, st)
	}
	handle := val
	if kind == "lists" {
		handle = strings.TrimSpace(rec[1])
	}
	user, domain, ok := parseHandle(handle)
	if !ok {
		return errors.New("bad account address")
	}
	acc, err := app.resolveAcct(ctx, user, domain)
	if err != nil {
		return err
	}
	if acc.ID == me.ID {
		return nil
	}
	switch kind {
	case "following": // Mastodon's columns: address, Show boosts, Notify on new posts
		if r := app.relationship(me.ID, acc.ID); !r.Following && !r.Requested {
			if err := app.Follow(me, acc); err != nil {
				return err
			}
		}
		var reblogs, notify *bool
		if len(rec) > 1 && strings.TrimSpace(rec[1]) != "" {
			b := strings.TrimSpace(rec[1]) != "false"
			reblogs = &b
		}
		if len(rec) > 2 && strings.TrimSpace(rec[2]) != "" {
			b := strings.TrimSpace(rec[2]) == "true"
			notify = &b
		}
		app.SetFollowOptions(me, acc, reblogs, notify)
		return nil
	case "blocks":
		return app.Block(me, acc)
	case "mutes": // Mastodon's second column is "Hide notifications"
		return app.MuteFor(me, acc, len(rec) < 2 || strings.TrimSpace(rec[1]) != "false", 0)
	case "lists":
		if r := app.relationship(me.ID, acc.ID); !r.Following && !r.Requested { // Mastodon's list import follows too
			if err := app.Follow(me, acc); err != nil {
				return err
			}
		}
		title := strings.TrimSpace(val)
		l := lists[title]
		if l == nil {
			if l, err = app.CreateList(me, title, "list", false); err != nil {
				return err
			}
			lists[title] = l
		}
		return app.AddToList(me, l, acc)
	}
	return nil
}

func (app *App) handleExport(w http.ResponseWriter, r *http.Request, p *Page) {
	file := r.PathValue("file")
	if file == "archive.zip" {
		app.handleArchive(w, r, p)
		return
	}
	kind, ok := exportFiles[file]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	if err := app.exportCSV(p.Me, kind, w); err != nil {
		http.Error(w, "export failed", 500)
	}
}

func (app *App) handleImport(w http.ResponseWriter, r *http.Request, p *Page) {
	f, _, err := r.FormFile("file")
	if err != nil {
		setFlash(w, "Choose a CSV file to import.")
	} else {
		defer f.Close()
		n, err := app.startImport(p.Me, r.FormValue("type"), io.LimitReader(f, 2<<20))
		if err != nil {
			setFlash(w, "Import failed: "+err.Error())
		} else {
			setFlash(w, fmt.Sprintf("Importing %d entries in the background; this page shows the progress.", n))
		}
	}
	http.Redirect(w, r, "/settings#import", http.StatusSeeOther)
}
