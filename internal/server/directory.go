package server

import (
	"net/http"
	"strconv"
)

// The profile directory: accounts that opted in (discoverable), local ones
// and remote ones known here, most recently active or newest first
// (Mastodon's /api/v1/directory and the web UI's /directory).

func (app *App) directory(order string, localOnly bool, limit, offset int) []*Account {
	where := `a.discoverable=1 AND a.suspended=0 AND ` + notLimitedSQL + ` AND a.moved_to='' AND a.actor_type IN ('Person','Service')
		AND NOT EXISTS(SELECT 1 FROM domain_blocks d WHERE d.domain=a.domain)`
	if localOnly {
		where += ` AND a.domain=''`
	}
	sort := `(SELECT MAX(s.created_at) FROM statuses s WHERE s.account_id=a.id AND s.visibility IN ('public','unlisted')) DESC`
	if order == "new" {
		sort = `a.created_at DESC`
	} else {
		where += ` AND EXISTS(SELECT 1 FROM statuses s WHERE s.account_id=a.id AND s.visibility IN ('public','unlisted'))`
	}
	accs, _ := app.accountsQuery(`SELECT `+accountCols+` FROM accounts a WHERE `+where+` ORDER BY `+sort+`, a.id LIMIT ? OFFSET ?`, limit, offset)
	return accs
}

func (app *App) apiDirectory(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	q := r.URL.Query()
	limit, offset := 40, 0
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, 80)
	}
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 {
		offset = n
	}
	local := q.Get("local") == "true" || q.Get("local") == "1"
	app.writeAccounts(w, app.directory(q.Get("order"), local, limit, offset))
}

func (app *App) handleDirectory(w http.ResponseWriter, r *http.Request, p *Page) {
	q := r.URL.Query()
	p.Title, p.Section, p.Heading = "Profile directory", "explore", "Profile directory"
	p.Tab = q.Get("order")
	if p.Tab != "new" {
		p.Tab = "active"
	}
	p.Query = q.Get("scope")
	if p.Query != "all" {
		p.Query = "local"
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	offset = max(offset, 0)
	p.Accounts = app.directory(p.Tab, p.Query == "local", 40, offset)
	if len(p.Accounts) == 40 {
		p.NextURL = "/directory?order=" + p.Tab + "&scope=" + p.Query + "&offset=" + strconv.Itoa(offset+40)
	}
	app.render(w, "directory", p)
}
