package server

import (
	"errors"
	"net/http"
	"time"
)

// Per-user domain blocks: a user hides a whole server. Its posts, boosts of
// its posts and its notifications disappear for that user, its accounts
// stop following them and new follows from there are refused (as on
// Mastodon). The server keeps federating with it for everyone else.

const maxUserDomainBlocks = 1000

func (app *App) BlockDomain(me *Account, domain string) error {
	d, ok := normalizeDomain(domain)
	if !ok && d != "" { // a server we know accounts from, e.g. one with a port
		ok = app.db.QueryRow(`SELECT 1 FROM accounts WHERE domain=? LIMIT 1`, d).Scan(new(int)) == nil
	}
	if !ok || d == app.cfg.Host {
		return errors.New("not a valid domain")
	}
	if app.countInt(`SELECT COUNT(*) FROM user_domain_blocks WHERE account_id=?`, me.ID) >= maxUserDomainBlocks {
		return errors.New("too many blocked domains")
	}
	if _, err := app.db.Exec(`INSERT OR IGNORE INTO user_domain_blocks (account_id, domain, created_at) VALUES (?,?,?)`,
		me.ID, d, time.Now().Unix()); err != nil {
		return err
	}
	followers, _ := app.accountsQuery(`SELECT `+accountCols+` FROM follows f JOIN accounts a ON a.id=f.account_id
		WHERE f.target_id=? AND a.domain=?`, me.ID, d)
	for _, f := range followers {
		app.RejectFollow(me, f)
	}
	return nil
}

func (app *App) UnblockDomain(me *Account, domain string) error {
	d, _ := normalizeDomain(domain)
	_, err := app.db.Exec(`DELETE FROM user_domain_blocks WHERE account_id=? AND domain=?`, me.ID, d)
	return err
}

// blocksDomain reports whether local account localID hides domain.
func (app *App) blocksDomain(localID, domain string) bool {
	if domain == "" {
		return false
	}
	return app.db.QueryRow(`SELECT 1 FROM user_domain_blocks WHERE account_id=? AND domain=?`, localID, domain).Scan(new(int)) == nil
}

func (app *App) userDomainBlocks(me *Account) []string {
	rows, err := app.db.Query(`SELECT domain FROM user_domain_blocks WHERE account_id=? ORDER BY domain`, me.ID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		rows.Scan(&d)
		out = append(out, d)
	}
	return out
}

// domainBlockSQL hides posts (and boosts of posts) from servers the viewer
// blocked; s is the statuses alias.
const domainBlockSQL = `NOT EXISTS(SELECT 1 FROM user_domain_blocks ub JOIN accounts da ON da.domain=ub.domain
		WHERE ub.account_id=? AND da.id=s.account_id)
	AND NOT (s.boost_of_id != '' AND EXISTS(SELECT 1 FROM statuses bo JOIN accounts da2 ON da2.id=bo.account_id
		JOIN user_domain_blocks ub2 ON ub2.domain=da2.domain WHERE bo.id=s.boost_of_id AND ub2.account_id=?))`

// notifDomainBlockSQL hides notifications from servers the recipient blocked; n is the notifications alias.
const notifDomainBlockSQL = `NOT EXISTS(SELECT 1 FROM user_domain_blocks ub JOIN accounts fa ON fa.domain=ub.domain
	WHERE ub.account_id=n.account_id AND fa.id=n.from_account_id)`

func (app *App) apiDomainBlocks(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []string{}
	out = append(out, app.userDomainBlocks(c.Me)...)
	apiJSON(w, 200, out)
}

func (app *App) apiBlockDomain(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	d := readParams(r).Get("domain")
	var err error
	if r.Method == http.MethodDelete {
		err = app.UnblockDomain(c.Me, d)
	} else {
		err = app.BlockDomain(c.Me, d)
	}
	if err != nil {
		apiError(w, 422, err.Error())
		return
	}
	apiJSON(w, 200, obj{})
}

// apiInstanceDomainBlocks lists the servers this instance blocks, as the
// About page does.
func (app *App) apiInstanceDomainBlocks(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	for _, b := range app.domainBlockList() {
		severity := "suspend"
		if b.Severity == "limit" {
			severity = "silence" // Mastodon's API name for limiting
		}
		out = append(out, obj{"domain": b.Domain, "digest": sha256Hex(b.Domain), "severity": severity, "comment": b.Reason})
	}
	apiJSON(w, 200, out)
}

func (app *App) handleDomainBlock(w http.ResponseWriter, r *http.Request, p *Page) {
	if d := r.FormValue("unblock"); d != "" {
		app.UnblockDomain(p.Me, d)
		setFlash(w, "Unblocked "+d+".")
	} else if err := app.BlockDomain(p.Me, r.FormValue("domain")); err != nil {
		setFlash(w, err.Error())
	} else {
		setFlash(w, "Blocked. Posts and notifications from there are hidden now.")
	}
	app.back(w, r, "/settings#domain-blocks")
}
