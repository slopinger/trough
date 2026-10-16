package server

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
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

	Statuses        []*Status
	Status          *Status
	Ancestors       []*Status
	Descendants     []*Status
	Account         *Account
	Rel             Relationship
	AccountNote     string // the viewer's private note on a profile
	Accounts        []*Account
	Notifications   []*Notification
	NextURL         string
	Query           string
	Advanced        bool // search: the query uses operators like from: or has:
	Tab             string
	Compose         *Compose
	Users           []*User
	DomainBlocks    []DomainBlock
	Stats           map[string]int
	Counts          struct{ Posts, Following, Followers int }
	Description     string
	Admins          []*Account
	Heading         string
	Empty           string
	Deliveries      []DeliveryInfo
	Pinned          []*Status
	Featured        []*Account // accounts the profile owner features
	QuotePolicy     string     // settings: who may quote my new posts
	Blocked         []*Account
	Filters         []*Filter
	Reports         []*Report
	Invites         []Invite
	Registrations   string
	Aliases         []*Account
	MovedTo         *Account
	Timeline        string // home/local/federated: enables live "new posts"
	OAuth           *authorizeReq
	Lists           []List
	List            *List
	Relays          []Relay
	Tag             string
	Rules           []Rule
	Announcements   []*Announcement // unread banner; all of them on /admin
	TrendTags       []TrendTag
	TrendLinks      []TrendLink
	Suggestions     []Suggestion
	Familiar        []*Account // people the viewer follows who follow this profile
	TagFollowed     bool
	TagFeatured     bool
	Codes           []string               // two-factor recovery codes, shown once
	TwoFactor       bool                   // settings: two-factor authentication is on
	Requests2       []*NotificationRequest // filtered notification requests
	PendingRequests int
	NotifPolicy     NotificationPolicy
	VapidKey        string // settings: for browser notifications
	Theme, TextSize string // appearance: auto/light/dark, normal/large
	FeaturedTags    []FeaturedTag
	FollowedTags    []string
	BlockedDomains  []string
	Import          *importState // settings: the running or last CSV import
	History         []HistoryEntry
	CustomEmojis    []CustomEmoji
	Scheduled       []*ScheduledStatus
	Apps            []ConnectedApp
	AI              bool   // AI writing available
	AIKeySource     string // admin: where the key comes from
	AIModel         string
}

type Compose struct {
	QuoteOf       *Status
	EditOf        *Status
	AIInstruction string
	ReplyTo       *Status
	Text          string
	Spoiler       string
	Visibility    string
	Sensitive     bool
	Language      string // keeps the choice when the form comes back with an error
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
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; media-src 'self' blob:; style-src 'self'; script-src 'self'; "+
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
					p.CustomEmojis = app.customEmojis()
					p.Announcements = app.unreadAnnouncements(acc)
					app.db.QueryRow(`SELECT theme, text_size FROM users WHERE account_id=?`, acc.ID).Scan(&p.Theme, &p.TextSize)
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
				limit = 4*maxMediaSize + 1<<20
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
	p.Rules = app.rules()
	p.Registrations = app.registrationMode()
	app.render(w, "about", p)
}
