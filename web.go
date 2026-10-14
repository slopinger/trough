package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	sessionCookie = "trough_session"
	flashCookie   = "trough_flash"
	sessionTTL    = 30 * 24 * time.Hour
)

// Page is the data passed to every template. Handlers fill in what they need.
type Page struct {
	Title    string
	Section  string
	Me       *Account
	Admin    bool
	CSRF     string
	Unread   int
	Requests int
	Flash    string
	Error    string
	Instance string
	Host     string
	Path     string

	Statuses      []*Status
	Status        *Status
	Ancestors     []*Status
	Descendants   []*Status
	Account       *Account
	Rel           Relationship
	Accounts      []*Account
	Notifications []*Notification
	NextURL       string
	Query         string
	Tab           string
	Compose       *Compose
	Users         []*User
	DomainBlocks  []DomainBlock
	Stats         map[string]int
	Counts        struct{ Posts, Following, Followers int }
	Description   string
	Admins        []*Account
	Heading       string
	Empty         string
	Deliveries    []DeliveryInfo
	AI            bool   // AI writing available
	AIKeySource   string // admin: where the key comes from
	AIModel       string
}

type Compose struct {
	AIInstruction string
	ReplyTo       *Status
	Text          string
	Spoiler       string
	Visibility    string
	Sensitive     bool
}

type DomainBlock struct {
	Domain, Reason string
	CreatedAt      time.Time
}

type DeliveryInfo struct {
	Inbox, LastError string
	Attempts         int
	NextTry          time.Time
}

type handler func(w http.ResponseWriter, r *http.Request, p *Page)

func (app *App) instanceTitle() string {
	if t := app.getSetting("title"); t != "" {
		return t
	}
	return app.cfg.Host
}

func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		// behind the local reverse proxy (Caddy), which sets X-Forwarded-For
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	return host
}

func (app *App) setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; media-src 'self'; style-src 'self'; script-src 'self'; "+
		"object-src 'none'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
}

// web wraps an HTML handler: loads the session, enforces CSRF on POSTs and
// optionally requires a logged-in user.
func (app *App) web(requireLogin bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app.setSecurityHeaders(w)
		p := &Page{Instance: app.instanceTitle(), Host: app.cfg.Host, Path: r.URL.Path}
		var sessID string
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			sessID = sha256Hex(c.Value)
			var accID, csrf string
			var expires int64
			err := app.db.QueryRow(`SELECT account_id, csrf, expires_at FROM sessions WHERE id=?`, sessID).Scan(&accID, &csrf, &expires)
			if err == nil && expires > time.Now().Unix() {
				if acc, err := app.AccountByID(accID); err == nil && !acc.Suspended {
					p.Me = acc
					p.CSRF = csrf
					p.Admin = app.isAdmin(acc.ID)
					p.Unread = app.unreadNotifications(acc.ID)
					p.Requests = app.followRequestCount(acc.ID)
					p.AI = app.aiEnabled()
					now := time.Now().Unix()
					app.db.Exec(`UPDATE users SET last_seen=? WHERE account_id=? AND last_seen < ?`, now, acc.ID, now-3600)
				}
			}
		}
		if c, err := r.Cookie(flashCookie); err == nil {
			if b, err := base64.RawURLEncoding.DecodeString(c.Value); err == nil {
				p.Flash = string(b)
			}
			http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1})
		}
		if r.Method == http.MethodPost {
			if o := r.Header.Get("Origin"); o != "" && o != app.cfg.BaseURL() {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			limit := int64(1 << 20)
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
				limit = 5*maxUploadSize + 1<<20
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			if p.Me != nil {
				if err := r.ParseMultipartForm(8 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
					http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
					return
				}
				if subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(p.CSRF)) != 1 {
					http.Error(w, "invalid form token, please reload the page", http.StatusForbidden)
					return
				}
			}
		}
		if requireLogin && p.Me == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			} else {
				http.Error(w, "login required", http.StatusUnauthorized)
			}
			return
		}
		h(w, r, p)
	}
}

func (app *App) render(w http.ResponseWriter, name string, p *Page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := app.tmpl.execute(w, name, p); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (app *App) renderStatus(w http.ResponseWriter, code int, name string, p *Page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := app.tmpl.execute(w, name, p); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (app *App) notFound(w http.ResponseWriter, p *Page) {
	p.Title = "Not found"
	app.renderStatus(w, http.StatusNotFound, "notfound", p)
}

func setFlash(w http.ResponseWriter, msg string) {
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: base64.RawURLEncoding.EncodeToString([]byte(msg)), Path: "/", MaxAge: 60, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// back redirects to a safe local "next"/Referer location.
func (app *App) back(w http.ResponseWriter, r *http.Request, fallback string) {
	target := r.FormValue("next")
	if target == "" {
		if ref, err := url.Parse(r.Referer()); err == nil && ref.Host == r.Host {
			target = ref.RequestURI()
			if ref.Fragment != "" {
				target += "#" + ref.Fragment
			}
		}
	}
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || strings.HasPrefix(target, "/\\") {
		target = fallback
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func isFetch(r *http.Request) bool { return r.Header.Get("X-Requested-With") == "fetch" }

// ---- auth ----

func (app *App) handleLoginForm(w http.ResponseWriter, r *http.Request, p *Page) {
	if p.Me != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	p.Title = "Log in"
	p.Query = r.URL.Query().Get("next")
	app.render(w, "login", p)
}

func (app *App) handleLogin(w http.ResponseWriter, r *http.Request, p *Page) {
	ip := clientIP(r)
	p.Title = "Log in"
	p.Query = r.FormValue("next")
	if app.logins.blocked(ip) {
		p.Error = "Too many failed attempts. Try again in a few minutes."
		app.renderStatus(w, http.StatusTooManyRequests, "login", p)
		return
	}
	acc, err := app.checkPassword(strings.TrimPrefix(strings.TrimSpace(r.FormValue("username")), "@"), r.FormValue("password"))
	if err != nil {
		app.logins.hit(ip)
		p.Error = "Invalid username or password."
		app.renderStatus(w, http.StatusUnauthorized, "login", p)
		return
	}
	token := randomToken(32)
	now := time.Now()
	if _, err := app.db.Exec(`INSERT INTO sessions (id,account_id,csrf,created_at,expires_at,user_agent) VALUES (?,?,?,?,?,?)`,
		sha256Hex(token), acc.ID, randomToken(16), now.Unix(), now.Add(sessionTTL).Unix(), truncateRunes(r.UserAgent(), 200)); err != nil {
		http.Error(w, "could not create session", 500)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: !app.cfg.Insecure,
		SameSite: http.SameSiteLaxMode, Expires: now.Add(sessionTTL),
	})
	next := r.FormValue("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (app *App) handleLogout(w http.ResponseWriter, r *http.Request, p *Page) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		app.db.Exec(`DELETE FROM sessions WHERE id=?`, sha256Hex(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---- timelines ----

func nextURL(r *http.Request, sts []*Status) string {
	if len(sts) < pageSize {
		return ""
	}
	q := r.URL.Query()
	q.Set("max_id", sts[len(sts)-1].ID)
	return r.URL.Path + "?" + q.Encode()
}

func (app *App) handleRoot(w http.ResponseWriter, r *http.Request, p *Page) {
	if p.Me != nil {
		app.handleHome(w, r, p)
		return
	}
	sts, err := app.localTimeline(nil, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title = app.instanceTitle()
	p.Section = "local"
	p.Statuses = sts
	p.NextURL = nextURL(r, sts)
	p.Description = app.getSetting("description")
	p.Stats = map[string]int{
		"users": app.countInt(`SELECT COUNT(*) FROM users`),
		"posts": app.countInt(`SELECT COUNT(*) FROM statuses WHERE local=1 AND boost_of_id=''`),
	}
	app.render(w, "landing", p)
}

func (app *App) handleHome(w http.ResponseWriter, r *http.Request, p *Page) {
	sts, err := app.homeTimeline(p.Me, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Section, p.Heading = "Home", "home", "Home"
	p.Statuses = sts
	p.NextURL = nextURL(r, sts)
	p.Compose = &Compose{Visibility: "public"}
	p.Empty = "Your home timeline is empty. Follow some people! Use search to find accounts on other servers, e.g. @someone@mastodon.social."
	app.render(w, "timeline", p)
}

func (app *App) handleLocal(w http.ResponseWriter, r *http.Request, p *Page) {
	sts, err := app.localTimeline(p.Me, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Section, p.Heading = "Local timeline", "local", "Local timeline"
	p.Statuses = sts
	p.NextURL = nextURL(r, sts)
	p.Empty = "Nobody here has posted anything public yet."
	app.render(w, "timeline", p)
}

func (app *App) handleFederated(w http.ResponseWriter, r *http.Request, p *Page) {
	sts, err := app.federatedTimeline(p.Me, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Section, p.Heading = "Federated timeline", "federated", "Federated timeline"
	p.Statuses = sts
	p.NextURL = nextURL(r, sts)
	p.Empty = "No public posts known yet."
	app.render(w, "timeline", p)
}

func (app *App) handleTag(w http.ResponseWriter, r *http.Request, p *Page) {
	tag := strings.ToLower(strings.TrimPrefix(r.PathValue("tag"), "#"))
	if wantsAP(r) {
		http.NotFound(w, r)
		return
	}
	sts, err := app.tagTimeline(p.Me, tag, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Heading = "#"+tag, "#"+tag
	p.Statuses = sts
	p.NextURL = nextURL(r, sts)
	p.Empty = "No public posts with this hashtag are known here."
	if p.Me != nil {
		p.Compose = &Compose{Visibility: "public", Text: "\n\n#" + tag}
	}
	app.render(w, "timeline", p)
}

// ---- statuses ----

func (app *App) handleThread(w http.ResponseWriter, r *http.Request, p *Page) {
	st, err := app.loadStatus(p.Me, r.PathValue("id"))
	if err != nil {
		app.notFound(w, p)
		return
	}
	app.showThread(w, r, p, st)
}

func (app *App) showThread(w http.ResponseWriter, r *http.Request, p *Page, st *Status) {
	if st.IsBoost() {
		http.Redirect(w, r, st.BoostOf.Path(), http.StatusFound)
		return
	}
	// if we are missing the parent, try to fetch it (logged-in users only)
	if p.Me != nil && st.InReplyToURI != "" && st.InReplyToID == "" {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		if parent, err := app.resolveStatus(ctx, st.InReplyToURI); err == nil {
			st.InReplyToID = parent.ID
		}
		cancel()
	}
	p.Status = st
	p.Ancestors, p.Descendants = app.thread(p.Me, st)
	p.Title = st.Account.Name() + ": " + truncateRunes(plainText(st.Content), 60)
	if p.Me != nil {
		p.Compose = app.replyCompose(p.Me, st)
	}
	app.render(w, "thread", p)
}

func (app *App) replyCompose(me *Account, st *Status) *Compose {
	c := &Compose{ReplyTo: st, Visibility: st.Visibility, Spoiler: st.Spoiler}
	var mentions []string
	seen := map[string]bool{me.ID: true}
	add := func(a *Account) {
		if a != nil && !seen[a.ID] {
			seen[a.ID] = true
			mentions = append(mentions, "@"+a.Acct())
		}
	}
	seenAcct := map[string]bool{strings.ToLower(me.Acct()): true}
	addAcct := func(acct string) {
		if k := strings.ToLower(acct); acct != "" && !seenAcct[k] {
			seenAcct[k] = true
			mentions = append(mentions, "@"+acct)
		}
	}
	add = func(a *Account) {
		if a != nil && !seen[a.ID] {
			seen[a.ID] = true
			addAcct(a.Acct())
		}
	}
	add(st.Account)
	for _, m := range st.Mentions {
		add(m)
	}
	// everyone mentioned in the post, including accounts we don't know yet
	for _, l := range app.mentionLinks(st.Content) {
		addAcct(l.Handle)
	}
	if len(mentions) > 0 {
		c.Text = strings.Join(mentions, " ") + " "
	}
	return c
}

func (app *App) handleComposePage(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section = "New post", "compose"
	p.Compose = &Compose{Visibility: "public"}
	if id := r.URL.Query().Get("reply_to"); id != "" {
		if st, err := app.loadStatus(p.Me, id); err == nil {
			if st.IsBoost() {
				st = st.BoostOf
			}
			p.Compose = app.replyCompose(p.Me, st)
		}
	}
	if t := r.URL.Query().Get("text"); t != "" {
		p.Compose.Text = t
	}
	app.render(w, "compose", p)
}

func (app *App) handlePost(w http.ResponseWriter, r *http.Request, p *Page) {
	in := PostInput{
		Text:       r.FormValue("text"),
		Spoiler:    r.FormValue("spoiler"),
		Visibility: r.FormValue("visibility"),
		Sensitive:  r.FormValue("sensitive") != "",
		InReplyTo:  r.FormValue("in_reply_to"),
	}
	fail := func(msg string) {
		p.Title, p.Error = "New post", msg
		p.Compose = &Compose{Text: in.Text, Spoiler: in.Spoiler, Visibility: in.Visibility, Sensitive: in.Sensitive}
		if in.InReplyTo != "" {
			p.Compose.ReplyTo, _ = app.loadStatus(p.Me, in.InReplyTo)
		}
		app.renderStatus(w, http.StatusBadRequest, "compose", p)
	}
	if r.MultipartForm != nil {
		for i := 0; i < 4; i++ {
			fhs := r.MultipartForm.File["media"+string(rune('0'+i))]
			if len(fhs) == 0 {
				continue
			}
			f, err := fhs[0].Open()
			if err != nil {
				fail("Could not read upload.")
				return
			}
			data, err := io.ReadAll(io.LimitReader(f, maxUploadSize+1))
			f.Close()
			if err != nil {
				fail("Could not read upload.")
				return
			}
			at, err := app.saveUpload(p.Me, data, r.FormValue("alt"+string(rune('0'+i))))
			if err != nil {
				fail(fhs[0].Filename + ": " + err.Error())
				return
			}
			in.MediaIDs = append(in.MediaIDs, at.ID)
		}
	}
	st, err := app.PostStatus(r.Context(), p.Me, in)
	if err != nil {
		fail(err.Error())
		return
	}
	if in.InReplyTo != "" {
		http.Redirect(w, r, "/s/"+in.InReplyTo+"#s-"+st.ID, http.StatusSeeOther)
		return
	}
	app.back(w, r, "/")
}

func (app *App) handleStatusAction(w http.ResponseWriter, r *http.Request, p *Page) {
	st, err := app.loadStatus(p.Me, r.PathValue("id"))
	if err != nil {
		http.Error(w, "post not found", http.StatusNotFound)
		return
	}
	target := st
	if st.IsBoost() {
		target = st.BoostOf
	}
	action := r.PathValue("action")
	switch action {
	case "like":
		err = app.Like(p.Me, target)
	case "unlike":
		err = app.Unlike(p.Me, target)
	case "boost":
		err = app.Boost(p.Me, target)
	case "unboost":
		err = app.Unboost(p.Me, target)
	case "delete":
		err = app.DeleteStatus(p.Me, st)
		if err == nil && !isFetch(r) {
			setFlash(w, "Post deleted.")
			if ref, perr := url.Parse(r.Referer()); perr == nil && strings.HasPrefix(ref.Path, "/s/"+st.ID) {
				http.Redirect(w, r, p.Me.Path(), http.StatusSeeOther)
				return
			}
		}
	default:
		http.NotFound(w, r)
		return
	}
	if isFetch(r) {
		if err != nil {
			writeJSON(w, "application/json", obj{"error": err.Error()})
			return
		}
		fresh, _ := app.loadStatus(p.Me, target.ID)
		res := obj{"ok": true}
		if fresh != nil {
			res["liked"], res["boosted"], res["likes"], res["boosts"] = fresh.Liked, fresh.Boosted, fresh.Likes, fresh.Boosts
		}
		writeJSON(w, "application/json", res)
		return
	}
	if err != nil {
		setFlash(w, err.Error())
	}
	app.back(w, r, "/")
}

// ---- profiles ----

func (app *App) handleProfileOrStatus(w http.ResponseWriter, r *http.Request, p *Page) {
	at := r.PathValue("at")
	if !strings.HasPrefix(at, "@") {
		app.notFound(w, p)
		return
	}
	user, domain, ok := parseHandle(at)
	if !ok {
		app.notFound(w, p)
		return
	}
	var acc *Account
	var err error
	if domain == "" || domain == app.cfg.Host {
		acc, err = app.LocalAccount(user)
	} else {
		acc, err = app.AccountByAcct(user, domain)
		if err != nil && p.Me != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			acc, err = app.resolveAcct(ctx, user, domain)
			cancel()
		}
	}
	if err != nil || acc.Suspended {
		app.notFound(w, p)
		return
	}
	sub := r.PathValue("id")
	if acc.IsLocal() && wantsAP(r) {
		if sub == "" {
			writeAP(w, app.actorJSON(acc))
		} else {
			app.apStatus(w, r, acc.Username, sub, false)
		}
		return
	}
	switch sub {
	case "":
		app.showProfile(w, r, p, acc)
	case "followers", "following":
		app.showFollows(w, r, p, acc, sub == "followers")
	default:
		st, err := app.loadStatus(p.Me, sub)
		if err != nil || st.AccountID != acc.ID {
			app.notFound(w, p)
			return
		}
		app.showThread(w, r, p, st)
	}
}

func (app *App) profileCommon(p *Page, acc *Account) {
	p.Account = acc
	if p.Me != nil {
		p.Rel = app.relationship(p.Me.ID, acc.ID)
	}
	if acc.IsLocal() {
		p.Counts.Posts = app.countInt(`SELECT COUNT(*) FROM statuses WHERE account_id=? AND boost_of_id=''`, acc.ID)
	} else {
		p.Counts.Posts = app.countInt(`SELECT COUNT(*) FROM statuses WHERE account_id=? AND boost_of_id='' AND visibility IN ('public','unlisted')`, acc.ID)
	}
	p.Counts.Following = app.countInt(`SELECT COUNT(*) FROM follows WHERE account_id=? AND accepted=1`, acc.ID)
	p.Counts.Followers = app.countInt(`SELECT COUNT(*) FROM follows WHERE target_id=? AND accepted=1`, acc.ID)
}

func (app *App) showProfile(w http.ResponseWriter, r *http.Request, p *Page, acc *Account) {
	if p.Me != nil && !acc.IsLocal() && r.URL.Query().Get("max_id") == "" &&
		app.countInt(`SELECT COUNT(*) FROM statuses WHERE account_id=?`, acc.ID) < 5 {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		app.backfillOutbox(ctx, acc)
		cancel()
	}
	app.profileCommon(p, acc)
	p.Tab = r.URL.Query().Get("tab")
	sts, err := app.accountTimeline(p.Me, acc, p.Tab == "replies", p.Tab == "media", r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Statuses = sts
	p.NextURL = nextURL(r, sts)
	p.Title = acc.Name() + " (@" + acc.Username + "@" + app.accountHost(acc) + ")"
	if p.Me != nil && p.Me.ID == acc.ID {
		p.Section = "profile"
	}
	app.render(w, "profile", p)
}

func (app *App) showFollows(w http.ResponseWriter, r *http.Request, p *Page, acc *Account, followers bool) {
	app.profileCommon(p, acc)
	list, err := app.followList(acc, followers, false, 0)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Accounts = list
	if followers {
		p.Tab, p.Title = "followers", "Followers of "+acc.Name()
	} else {
		p.Tab, p.Title = "following", acc.Name()+" follows"
	}
	app.render(w, "follows", p)
}

func (app *App) handleAccountAction(w http.ResponseWriter, r *http.Request, p *Page) {
	target, err := app.AccountByID(r.PathValue("id"))
	if err != nil || target.ActorType == "Application" && target.IsLocal() {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	switch r.PathValue("action") {
	case "follow":
		err = app.Follow(p.Me, target)
		if err == nil && !target.IsLocal() {
			setFlash(w, "Follow request sent to @"+target.Acct()+".")
		}
	case "unfollow":
		err = app.Unfollow(p.Me, target)
	case "block":
		err = app.Block(p.Me, target)
	case "unblock":
		err = app.Unblock(p.Me, target)
	case "accept":
		err = app.AcceptFollow(p.Me, target)
	case "reject", "remove_follower":
		err = app.RejectFollow(p.Me, target)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		setFlash(w, err.Error())
	}
	app.back(w, r, target.Path())
}

func (app *App) handleFollowRequests(w http.ResponseWriter, r *http.Request, p *Page) {
	list, err := app.followList(p.Me, true, true, 0)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Section = "Follow requests", "requests"
	p.Accounts = list
	app.render(w, "requests", p)
}

func (app *App) handleNotifications(w http.ResponseWriter, r *http.Request, p *Page) {
	ns, err := app.notifications(p.Me, r.URL.Query().Get("max_id"))
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Title, p.Section = "Notifications", "notifications"
	p.Notifications = ns
	if len(ns) == pageSize*2 {
		p.NextURL = "/notifications?max_id=" + ns[len(ns)-1].ID
	}
	if r.URL.Query().Get("max_id") == "" {
		app.db.Exec(`UPDATE notifications SET read=1 WHERE account_id=? AND read=0`, p.Me.ID)
	}
	app.render(w, "notifications", p)
}

func (app *App) handleClearNotifications(w http.ResponseWriter, r *http.Request, p *Page) {
	app.db.Exec(`DELETE FROM notifications WHERE account_id=?`, p.Me.ID)
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}

// ---- search ----

func (app *App) handleSearch(w http.ResponseWriter, r *http.Request, p *Page) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	p.Title, p.Section, p.Query = "Search", "search", q
	if q == "" {
		app.render(w, "search", p)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	switch {
	case strings.HasPrefix(q, "#") && !strings.ContainsAny(q, " "):
		http.Redirect(w, r, "/tags/"+url.PathEscape(strings.ToLower(q[1:])), http.StatusFound)
		return
	case strings.HasPrefix(q, "https://") || strings.HasPrefix(q, "http://"):
		acc, st, err := app.resolveURL(ctx, q)
		if err == nil && acc != nil {
			http.Redirect(w, r, acc.Path(), http.StatusFound)
			return
		}
		if err == nil && st != nil {
			http.Redirect(w, r, st.Path(), http.StatusFound)
			return
		}
		p.Error = "Could not find anything at that address."
		if err != nil {
			p.Error += " (" + err.Error() + ")"
		}
	case strings.Count(q, "@") >= 1 && !strings.Contains(q, " ") && strings.Contains(strings.TrimPrefix(q, "@"), "@"):
		user, domain, ok := parseHandle(q)
		if ok {
			acc, err := app.resolveAcct(ctx, user, domain)
			if err == nil {
				http.Redirect(w, r, acc.Path(), http.StatusFound)
				return
			}
			p.Error = "Could not find " + q + " (" + err.Error() + ")."
		}
	}
	like := "%" + strings.NewReplacer("%", `\%`, "_", `\_`).Replace(strings.TrimPrefix(q, "@")) + "%"
	accs, _ := app.accountsQuery(`SELECT `+accountCols+` FROM accounts a WHERE a.suspended=0 AND a.actor_type != 'Application'
		AND (a.username LIKE ? ESCAPE '\' OR a.display_name LIKE ? ESCAPE '\') ORDER BY a.domain != '', a.fetched_at DESC LIMIT 20`, like, like)
	p.Accounts = accs
	sts, _ := app.queryStatuses(p.Me, `s.boost_of_id='' AND s.visibility IN ('public','unlisted') AND s.content LIKE ? ESCAPE '\'`, []any{like}, "", 20, false)
	p.Statuses = sts
	app.render(w, "search", p)
}

func (app *App) handleAuthorizeInteraction(w http.ResponseWriter, r *http.Request, p *Page) {
	uri := strings.TrimSpace(r.URL.Query().Get("uri"))
	if uri == "" {
		http.Redirect(w, r, "/search", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/search?q="+url.QueryEscape(uri), http.StatusFound)
}

// ---- settings ----

func (app *App) handleSettings(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section = "Settings", "settings"
	p.Account = p.Me
	app.render(w, "settings", p)
}

func (app *App) readImageField(r *http.Request, field string) ([]byte, bool) {
	if r.MultipartForm == nil {
		return nil, false
	}
	fhs := r.MultipartForm.File[field]
	if len(fhs) == 0 || fhs[0].Size == 0 {
		return nil, false
	}
	f, err := fhs[0].Open()
	if err != nil {
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUploadSize+1))
	return data, err == nil
}

func (app *App) replaceImage(me *Account, old string, data []byte) (string, error) {
	at, err := app.saveUpload(me, data, "")
	if err != nil {
		return "", err
	}
	app.db.Exec(`DELETE FROM attachments WHERE id=?`, at.ID) // not a post attachment
	if strings.HasPrefix(old, app.cfg.BaseURL()+"/media/") {
		os.Remove(filepath.Join(app.mediaDir, filepath.Base(old)))
	}
	return at.URL, nil
}

func (app *App) handleSettingsProfile(w http.ResponseWriter, r *http.Request, p *Page) {
	me := p.Me
	me.DisplayName = truncateRunes(strings.TrimSpace(r.FormValue("display_name")), 100)
	me.NoteSource = truncateRunes(strings.TrimSpace(r.FormValue("note")), 2000)
	me.Note = app.formatBio(r.Context(), me.NoteSource)
	me.Locked = r.FormValue("locked") != ""
	if r.FormValue("bot") != "" {
		me.ActorType = "Service"
	} else {
		me.ActorType = "Person"
	}
	me.Fields = nil
	for i := 0; i < 4; i++ {
		n := truncateRunes(strings.TrimSpace(r.FormValue("field_name"+string(rune('0'+i)))), 100)
		v := truncateRunes(strings.TrimSpace(r.FormValue("field_value"+string(rune('0'+i)))), 500)
		if n != "" || v != "" {
			me.Fields = append(me.Fields, Field{Name: n, Value: fieldHTML(v)})
		}
	}
	var errs []string
	if data, ok := app.readImageField(r, "avatar"); ok {
		if u, err := app.replaceImage(me, me.Avatar, data); err == nil {
			me.Avatar = u
		} else {
			errs = append(errs, "avatar: "+err.Error())
		}
	} else if r.FormValue("remove_avatar") != "" {
		app.replaceRemove(me.Avatar)
		me.Avatar = ""
	}
	if data, ok := app.readImageField(r, "header"); ok {
		if u, err := app.replaceImage(me, me.Header, data); err == nil {
			me.Header = u
		} else {
			errs = append(errs, "header: "+err.Error())
		}
	} else if r.FormValue("remove_header") != "" {
		app.replaceRemove(me.Header)
		me.Header = ""
	}
	if err := app.UpdateProfile(me); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		setFlash(w, "Saved, but: "+strings.Join(errs, "; "))
	} else {
		setFlash(w, "Profile saved.")
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (app *App) replaceRemove(old string) {
	if strings.HasPrefix(old, app.cfg.BaseURL()+"/media/") {
		os.Remove(filepath.Join(app.mediaDir, filepath.Base(old)))
	}
}

// fieldHTML renders a profile field value: a bare URL becomes a link.
func fieldHTML(v string) string {
	if (strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://")) && !strings.ContainsAny(v, " \t\n") {
		return linkHTML(v)
	}
	return htmlEscape(v)
}

func (app *App) handleSettingsPassword(w http.ResponseWriter, r *http.Request, p *Page) {
	if _, err := app.checkPassword(p.Me.Username, r.FormValue("current")); err != nil {
		setFlash(w, "Current password is wrong.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if r.FormValue("new") != r.FormValue("confirm") {
		setFlash(w, "The new passwords don't match.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if err := app.SetPassword(p.Me.ID, r.FormValue("new")); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	setFlash(w, "Password changed. Please log in again.")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- about ----

func (app *App) handleAbout(w http.ResponseWriter, r *http.Request, p *Page) {
	if wantsAP(r) {
		app.handleInstanceActorDoc(w, r)
		return
	}
	p.Title, p.Section = "About", "about"
	p.Description = app.getSetting("description")
	p.Admins, _ = app.accountsQuery(`SELECT ` + accountCols + ` FROM accounts a JOIN users u ON u.account_id=a.id WHERE u.admin=1 ORDER BY a.username`)
	p.Stats = map[string]int{
		"users":     app.countInt(`SELECT COUNT(*) FROM users`),
		"posts":     app.countInt(`SELECT COUNT(*) FROM statuses WHERE local=1 AND boost_of_id=''`),
		"instances": app.countInt(`SELECT COUNT(DISTINCT domain) FROM accounts WHERE domain != ''`),
	}
	p.DomainBlocks = app.domainBlockList()
	app.render(w, "about", p)
}
