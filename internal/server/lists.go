package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"git.freifick.net/slop/trough/internal/ulid"
)

// ---- lists ----

type List struct {
	ID, Title, RepliesPolicy string
	Exclusive                bool
	Members                  int
}

func (app *App) lists(me *Account) []List {
	rows, err := app.db.Query(`SELECT l.id, l.title, l.replies_policy, l.exclusive, (SELECT COUNT(*) FROM list_accounts la WHERE la.list_id=l.id)
		FROM lists l WHERE l.account_id=? ORDER BY l.title COLLATE NOCASE`, me.ID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []List
	for rows.Next() {
		var l List
		rows.Scan(&l.ID, &l.Title, &l.RepliesPolicy, &l.Exclusive, &l.Members)
		out = append(out, l)
	}
	return out
}

func (app *App) listByID(me *Account, id string) (*List, error) {
	l := &List{}
	err := app.db.QueryRow(`SELECT id, title, replies_policy, exclusive FROM lists WHERE id=? AND account_id=?`, id, me.ID).
		Scan(&l.ID, &l.Title, &l.RepliesPolicy, &l.Exclusive)
	if err != nil {
		return nil, errNotFound
	}
	return l, nil
}

var repliesPolicies = map[string]bool{"list": true, "followed": true, "none": true}

func (app *App) CreateList(me *Account, title, policy string, exclusive bool) (*List, error) {
	title = truncateRunes(strings.TrimSpace(title), 100)
	if title == "" {
		return nil, errors.New("give the list a name")
	}
	if !repliesPolicies[policy] {
		policy = "list"
	}
	if app.countInt(`SELECT COUNT(*) FROM lists WHERE account_id=?`, me.ID) >= 50 {
		return nil, errors.New("too many lists")
	}
	l := &List{ID: ulid.New(), Title: title, RepliesPolicy: policy, Exclusive: exclusive}
	_, err := app.db.Exec(`INSERT INTO lists (id, account_id, title, replies_policy, exclusive, created_at) VALUES (?,?,?,?,?,?)`,
		l.ID, me.ID, l.Title, l.RepliesPolicy, l.Exclusive, time.Now().Unix())
	return l, err
}

func (app *App) UpdateList(me *Account, l *List, title, policy string, exclusive bool) error {
	if t := truncateRunes(strings.TrimSpace(title), 100); t != "" {
		l.Title = t
	}
	if repliesPolicies[policy] {
		l.RepliesPolicy = policy
	}
	l.Exclusive = exclusive
	_, err := app.db.Exec(`UPDATE lists SET title=?, replies_policy=?, exclusive=? WHERE id=? AND account_id=?`, l.Title, l.RepliesPolicy, l.Exclusive, l.ID, me.ID)
	return err
}

func (app *App) AddToList(me *Account, l *List, acc *Account) error {
	if !app.relationship(me.ID, acc.ID).Following {
		return errors.New("you can only add accounts you follow")
	}
	_, err := app.db.Exec(`INSERT OR IGNORE INTO list_accounts (list_id, account_id) VALUES (?,?)`, l.ID, acc.ID)
	return err
}

func (app *App) listMembers(l *List) []*Account {
	accs, _ := app.accountsQuery(`SELECT `+accountCols+` FROM list_accounts la JOIN accounts a ON a.id=la.account_id WHERE la.list_id=? ORDER BY a.username`, l.ID)
	return accs
}

func listSQL(v *Account, l *List) (string, []any) {
	where := `s.account_id IN (SELECT account_id FROM list_accounts WHERE list_id=?)`
	args := []any{l.ID}
	switch l.RepliesPolicy {
	case "none":
		where += ` AND (s.in_reply_to_uri='' OR s.in_reply_to_account_id=s.account_id)`
	case "list":
		where += ` AND (s.in_reply_to_uri='' OR s.in_reply_to_account_id=s.account_id OR s.in_reply_to_account_id=?
			OR s.in_reply_to_account_id IN (SELECT account_id FROM list_accounts WHERE list_id=?))`
		args = append(args, v.ID, l.ID)
	default: // followed
		where += ` AND (s.in_reply_to_uri='' OR s.in_reply_to_account_id=s.account_id OR s.in_reply_to_account_id=?
			OR s.in_reply_to_account_id IN (SELECT target_id FROM follows WHERE account_id=? AND accepted=1))`
		args = append(args, v.ID, v.ID)
	}
	mute, margs := muteSQL(v)
	return where + ` AND ` + mute, append(args, margs...)
}

func (app *App) handleLists(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section = "Lists", "lists"
	p.Lists = app.lists(p.Me)
	app.render(w, "lists", p)
}

func (app *App) handleCreateList(w http.ResponseWriter, r *http.Request, p *Page) {
	l, err := app.CreateList(p.Me, r.FormValue("title"), r.FormValue("replies_policy"), r.FormValue("exclusive") != "")
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/lists", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/lists/"+l.ID, http.StatusSeeOther)
}

func (app *App) handleList(w http.ResponseWriter, r *http.Request, p *Page) {
	l, err := app.listByID(p.Me, r.PathValue("id"))
	if err != nil {
		app.notFound(w, p)
		return
	}
	sts, err := app.listTimeline(p.Me, l, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Section, p.Heading = l.Title, "lists", l.Title
	p.List = l
	p.Accounts = app.listMembers(l)
	p.NextURL = nextURL(r, sts)
	p.Statuses = app.applyFilters(p.Me, sts, "home")
	p.Empty = "No posts from the people in this list yet. Add accounts you follow below or from their profile."
	app.render(w, "list", p)
}

func (app *App) handleListAction(w http.ResponseWriter, r *http.Request, p *Page) {
	l, err := app.listByID(p.Me, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch r.PathValue("action") {
	case "update":
		err = app.UpdateList(p.Me, l, r.FormValue("title"), r.FormValue("replies_policy"), r.FormValue("exclusive") != "")
	case "delete":
		app.db.Exec(`DELETE FROM lists WHERE id=? AND account_id=?`, l.ID, p.Me.ID)
		http.Redirect(w, r, "/lists", http.StatusSeeOther)
		return
	case "add":
		var acc *Account
		if id := r.FormValue("account"); id != "" {
			acc, err = app.AccountByID(id)
		} else {
			acc, err = app.resolveHandleOrURL(r.Context(), r.FormValue("handle"))
		}
		if err == nil {
			err = app.AddToList(p.Me, l, acc)
		}
	case "remove":
		app.db.Exec(`DELETE FROM list_accounts WHERE list_id=? AND account_id=?`, l.ID, r.FormValue("account"))
	}
	if err != nil {
		setFlash(w, err.Error())
	}
	app.back(w, r, "/lists/"+l.ID)
}

func apiList(l *List) obj {
	return obj{"id": l.ID, "title": l.Title, "replies_policy": l.RepliesPolicy, "exclusive": l.Exclusive}
}

func (app *App) apiLists(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	for _, l := range app.lists(c.Me) {
		out = append(out, apiList(&l))
	}
	apiJSON(w, 200, out)
}

func (app *App) apiCreateList(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	p := readParams(r)
	l, err := app.CreateList(c.Me, p.Get("title"), p.Get("replies_policy"), p.Bool("exclusive"))
	if err != nil {
		apiError(w, 422, err.Error())
		return
	}
	apiJSON(w, 200, apiList(l))
}

func (app *App) apiList1(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	l, err := app.listByID(c.Me, r.PathValue("id"))
	if err != nil {
		apiError(w, 404, "Record not found")
		return
	}
	switch r.Method {
	case http.MethodPut:
		p := readParams(r)
		excl := l.Exclusive
		if p.Has("exclusive") {
			excl = p.Bool("exclusive")
		}
		app.UpdateList(c.Me, l, p.Get("title"), p.Get("replies_policy"), excl)
	case http.MethodDelete:
		app.db.Exec(`DELETE FROM lists WHERE id=?`, l.ID)
		apiJSON(w, 200, obj{})
		return
	}
	apiJSON(w, 200, apiList(l))
}

func (app *App) apiListAccounts(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	l, err := app.listByID(c.Me, r.PathValue("id"))
	if err != nil {
		apiError(w, 404, "Record not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		app.writeAccounts(w, app.listMembers(l))
		return
	case http.MethodPost:
		for _, id := range readParams(r).List("account_ids") {
			acc, err := app.AccountByID(id)
			if err == nil {
				err = app.AddToList(c.Me, l, acc)
			}
			if err != nil {
				apiError(w, 422, err.Error())
				return
			}
		}
	case http.MethodDelete:
		for _, id := range readParams(r).List("account_ids") {
			app.db.Exec(`DELETE FROM list_accounts WHERE list_id=? AND account_id=?`, l.ID, id)
		}
	}
	apiJSON(w, 200, obj{})
}

func (app *App) apiListTimeline(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	l, err := app.listByID(c.Me, r.PathValue("id"))
	if err != nil {
		apiError(w, 404, "Record not found")
		return
	}
	pq := pageParams(r, 20, 40)
	where, args := listSQL(c.Me, l)
	sts, err := app.pageStatuses(c.Me, where, args, pq)
	if err != nil {
		apiError(w, 500, "db error")
		return
	}
	app.writeStatuses(w, r, c.Me, app.applyFilters(c.Me, sts, "home"), pq)
}

func (app *App) apiAccountLists(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	rows, err := app.db.Query(`SELECT l.id, l.title, l.replies_policy, l.exclusive FROM lists l JOIN list_accounts la ON la.list_id=l.id
		WHERE l.account_id=? AND la.account_id=?`, c.Me.ID, r.PathValue("id"))
	if err == nil {
		for rows.Next() {
			var l List
			rows.Scan(&l.ID, &l.Title, &l.RepliesPolicy, &l.Exclusive)
			out = append(out, apiList(&l))
		}
		rows.Close()
	}
	apiJSON(w, 200, out)
}
