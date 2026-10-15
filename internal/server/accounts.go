package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"git.freifick.net/slop/trough/internal/ap"
	"git.freifick.net/slop/trough/internal/ulid"
)

// ---- account migration ----

// AddAlias records that another account belongs to the same person, which
// that account needs before it can Move here.
func (app *App) AddAlias(ctx context.Context, me *Account, handle string) error {
	other, err := app.resolveHandleOrURL(ctx, handle)
	if err != nil {
		return err
	}
	if other.ID == me.ID {
		return errors.New("that's this account")
	}
	for _, a := range me.AlsoKnownAs {
		if a == other.URI {
			return nil
		}
	}
	if len(me.AlsoKnownAs) >= 10 {
		return errors.New("too many aliases")
	}
	me.AlsoKnownAs = append(me.AlsoKnownAs, other.URI)
	return app.UpdateProfile(me)
}

func (app *App) RemoveAlias(me *Account, uri string) error {
	var keep []string
	for _, a := range me.AlsoKnownAs {
		if a != uri {
			keep = append(keep, a)
		}
	}
	me.AlsoKnownAs = keep
	return app.UpdateProfile(me)
}

func (app *App) resolveHandleOrURL(ctx context.Context, s string) (*Account, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		acc, _, err := app.resolveURL(ctx, s)
		if err != nil || acc == nil {
			return nil, errors.New("no account found at that address")
		}
		return acc, nil
	}
	user, domain, ok := parseHandle(s)
	if !ok {
		return nil, errors.New("enter a handle like @name@server")
	}
	acc, err := app.resolveAcct(ctx, user, domain)
	if err != nil {
		return nil, errors.New("could not find " + s)
	}
	return acc, nil
}

// MoveTo moves this account's followers to another account, which must list
// this account as an alias. The old account stays, marked as moved.
func (app *App) MoveTo(ctx context.Context, me *Account, handle string) (*Account, error) {
	target, err := app.resolveHandleOrURL(ctx, handle)
	if err != nil {
		return nil, err
	}
	if target.ID == me.ID {
		return nil, errors.New("you can't move to the same account")
	}
	if !target.IsLocal() {
		if fresh, err := app.fetchAccount(ctx, target.URI); err == nil {
			target = fresh
		}
	}
	if !contains(target.AlsoKnownAs, me.URI) {
		return nil, errors.New("first add " + "@" + me.Username + "@" + app.cfg.Host + " as an alias (\"also known as\") on the new account")
	}
	me.MovedTo = target.URI
	if err := app.saveAccount(me); err != nil {
		return nil, err
	}
	inboxes := app.followerInboxes(me.ID)
	app.enqueue(me, inboxes, app.updateActorActivity(me))
	app.enqueue(me, inboxes, withContext(obj{
		"id": me.URI + "#moves/" + ulid.New(), "type": "Move", "actor": me.URI, "object": me.URI, "target": target.URI,
	}))
	app.moveLocalFollowers(me, target)
	return target, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// moveLocalFollowers makes local followers of old follow target instead.
func (app *App) moveLocalFollowers(old, target *Account) {
	followers, err := app.accountsQuery(`SELECT `+accountCols+` FROM follows f JOIN accounts a ON a.id=f.account_id
		WHERE f.target_id=? AND f.accepted=1 AND a.domain='' AND EXISTS(SELECT 1 FROM users u WHERE u.account_id=a.id)`, old.ID)
	if err != nil {
		return
	}
	for _, f := range followers {
		if f.ID == target.ID || app.isBlockedBy(f.ID, target.ID) {
			continue
		}
		app.Unfollow(f, old)
		if err := app.Follow(f, target); err != nil {
			log.Printf("move: %s -> %s for %s: %v", old.URI, target.URI, f.Username, err)
		}
		app.notify(f.ID, "move", old.ID, "")
	}
}

// inMove handles a remote account announcing it moved.
func (app *App) inMove(ctx context.Context, actor *Account, act obj) error {
	if ap.ID(act["object"]) != actor.URI {
		return errors.New("move of another actor")
	}
	targetIRI := ap.ID(act["target"])
	if targetIRI == "" || targetIRI == actor.URI {
		return nil
	}
	var target *Account
	var err error
	if app.isLocalIRI(targetIRI) {
		target, err = app.AccountByURI(targetIRI)
	} else {
		target, err = app.fetchAccount(ctx, targetIRI)
	}
	if err != nil {
		return err
	}
	if !contains(target.AlsoKnownAs, actor.URI) {
		return errors.New("move target does not list the old account as an alias")
	}
	actor.MovedTo = target.URI
	app.db.Exec(`UPDATE accounts SET moved_to=? WHERE id=?`, target.URI, actor.ID)
	app.moveLocalFollowers(actor, target)
	return nil
}

// ---- registrations ----

func (app *App) registrationMode() string {
	switch m := app.getSetting("registrations"); m {
	case "open", "invite":
		return m
	}
	return "closed"
}

func (app *App) CreateInvite(me *Account, maxUses int, valid time.Duration) (string, error) {
	if maxUses < 1 || maxUses > 100 {
		maxUses = 1
	}
	code := randomToken(9)
	var exp int64
	if valid > 0 {
		exp = time.Now().Add(valid).Unix()
	}
	_, err := app.db.Exec(`INSERT INTO invites (code, created_by, max_uses, expires_at, created_at) VALUES (?,?,?,?,?)`,
		code, me.ID, maxUses, exp, time.Now().Unix())
	return code, err
}

func (app *App) inviteValid(code string) bool {
	var uses, max int
	var exp int64
	if app.db.QueryRow(`SELECT uses, max_uses, expires_at FROM invites WHERE code=?`, code).Scan(&uses, &max, &exp) != nil {
		return false
	}
	return uses < max && (exp == 0 || time.Now().Unix() < exp)
}

type Invite struct {
	Code             string
	Uses, MaxUses    int
	Expires, Created time.Time
}

func (app *App) invites() []Invite {
	rows, err := app.db.Query(`SELECT code, uses, max_uses, expires_at, created_at FROM invites ORDER BY created_at DESC LIMIT 50`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var i Invite
		var exp, created int64
		rows.Scan(&i.Code, &i.Uses, &i.MaxUses, &exp, &created)
		if exp > 0 {
			i.Expires = time.Unix(exp, 0)
		}
		i.Created = time.Unix(created, 0)
		out = append(out, i)
	}
	return out
}

var signups = newRateLimiter(5, time.Hour)

func (app *App) handleSignupForm(w http.ResponseWriter, r *http.Request, p *Page) {
	if p.Me != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	p.Title = "Sign up"
	p.Query = r.URL.Query().Get("invite")
	mode := app.registrationMode()
	if mode == "closed" || (mode == "invite" && !app.inviteValid(p.Query)) {
		p.Error = "Sign-ups are closed. You need a valid invite link from someone on this server."
		if mode == "closed" {
			p.Error = "Sign-ups are closed on this server."
		}
		app.renderStatus(w, http.StatusForbidden, "signup", p)
		return
	}
	app.render(w, "signup", p)
}

func (app *App) handleSignup(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title = "Sign up"
	code := r.FormValue("invite")
	p.Query = code
	mode := app.registrationMode()
	if mode == "closed" || (mode == "invite" && !app.inviteValid(code)) {
		p.Error = "Sign-ups are closed or the invite is no longer valid."
		app.renderStatus(w, http.StatusForbidden, "signup", p)
		return
	}
	ip := clientIP(r)
	if signups.blocked(ip) {
		p.Error = "Too many sign-ups from your network. Try again later."
		app.renderStatus(w, http.StatusTooManyRequests, "signup", p)
		return
	}
	if r.FormValue("password") != r.FormValue("confirm") {
		p.Error = "The passwords don't match."
		app.renderStatus(w, http.StatusBadRequest, "signup", p)
		return
	}
	if r.FormValue("rules") == "" {
		p.Error = "Please agree to the server rules."
		app.renderStatus(w, http.StatusBadRequest, "signup", p)
		return
	}
	acc, err := app.CreateLocalUser(strings.TrimSpace(r.FormValue("username")), r.FormValue("password"), false)
	if err != nil {
		p.Error = err.Error()
		app.renderStatus(w, http.StatusBadRequest, "signup", p)
		return
	}
	signups.hit(ip)
	if code != "" {
		app.db.Exec(`UPDATE invites SET uses=uses+1 WHERE code=?`, code)
	}
	app.notifyAdmins(acc.ID, "signup")
	app.startSession(w, r, acc)
	setFlash(w, "Welcome to "+app.instanceTitle()+"! Set up your profile, then find people to follow.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

var domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// normalizeDomain turns "https://Blog.Example/" into "blog.example".
// Internationalized names have to be given in punycode (xn--...).
func normalizeDomain(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s, len(s) <= 253 && domainRE.MatchString(s)
}

// attributionDomains checks the websites that may credit an account as
// the author of their pages (fediverse:creator, Mastodon 4.4).
func attributionDomains(list []string) ([]string, error) {
	var out []string
	for _, d := range list {
		if strings.TrimSpace(d) == "" {
			continue
		}
		n, ok := normalizeDomain(d)
		if !ok {
			return nil, fmt.Errorf("%q is not a domain", d)
		}
		if !contains(out, n) {
			out = append(out, n)
		}
	}
	if len(out) > 10 {
		return nil, errors.New("at most 10 attribution domains")
	}
	return out, nil
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
