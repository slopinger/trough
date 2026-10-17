package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"

	"git.freifick.net/slop/trough/internal/ulid"
)

var errNotFound = errors.New("not found")

type Field struct {
	Name  string `json:"name"`
	Value string `json:"value"` // HTML
	// VerifiedAt is when the linked page was found to link back with rel="me" (unix time, 0 = not verified).
	VerifiedAt int64 `json:"verified_at,omitempty"`
}

type Emoji struct {
	Shortcode string `json:"shortcode"`
	URL       string `json:"url"`
}

type Account struct {
	ID, Username, Domain, URI, URL     string
	DisplayName, Note, NoteSource      string
	Avatar, Header                     string
	Inbox, SharedInbox, Outbox         string
	FollowersURI, FollowingURI         string
	PublicKeyID, PublicKey, PrivateKey string
	ActorType                          string
	Locked                             bool
	Fields                             []Field
	Emojis                             []Emoji
	CreatedAt, FetchedAt               time.Time
	Suspended                          bool
	FeaturedURI                        string
	AlsoKnownAs                        []string
	MovedTo                            string   // actor IRI the account moved to
	AttributionDomains                 []string // websites that may credit the account as author
	Discoverable                       bool     // listed in the profile directory and suggestions
	HideCollections                    bool     // followers and follows are only shown as counts
	Indexable                          bool     // public posts may be found by full-text search (toot:indexable)
	NoIndex                            bool     // local: asks search engines not to index the profile and posts
	Limited                            bool     // limited by our moderators (limit.go)
	DomainLimited                      bool     // its server is limited (domainlimit.go); not stored
	ForceSensitive                     bool     // moderators marked all its media sensitive; kept out of saveAccount
}

func (a *Account) IsLocal() bool { return a.Domain == "" }
func (a *Account) IsBot() bool   { return a.ActorType == "Service" || a.ActorType == "Application" }

// Acct is user for local accounts and user@domain for remote ones.
func (a *Account) Acct() string {
	if a.Domain == "" {
		return a.Username
	}
	return a.Username + "@" + a.Domain
}

func (a *Account) Name() string {
	if strings.TrimSpace(a.DisplayName) != "" {
		return a.DisplayName
	}
	return a.Username
}

// Path is the profile page of the account in our web interface.
func (a *Account) Path() string { return "/@" + a.Acct() }

func (a *Account) DeliveryInbox() string {
	if a.SharedInbox != "" {
		return a.SharedInbox
	}
	return a.Inbox
}

const accountCols = `a.id,a.username,a.domain,a.uri,a.url,a.display_name,a.note,a.note_source,a.avatar,a.header,
a.inbox,a.shared_inbox,a.outbox,a.followers_uri,a.following_uri,a.public_key_id,a.public_key,a.private_key,
a.actor_type,a.locked,a.fields,a.emojis,a.created_at,a.fetched_at,a.suspended,a.featured_uri,a.also_known_as,a.moved_to,
a.attribution_domains,a.discoverable,a.hide_collections,a.indexable,a.noindex,a.limited,a.force_sensitive,` + domainLimitedSQL

type scanner interface{ Scan(...any) error }

func scanAccount(r scanner) (*Account, error) {
	a := &Account{}
	var fields, emojis, aka, attr string
	var created, fetched int64
	err := r.Scan(&a.ID, &a.Username, &a.Domain, &a.URI, &a.URL, &a.DisplayName, &a.Note, &a.NoteSource, &a.Avatar, &a.Header,
		&a.Inbox, &a.SharedInbox, &a.Outbox, &a.FollowersURI, &a.FollowingURI, &a.PublicKeyID, &a.PublicKey, &a.PrivateKey,
		&a.ActorType, &a.Locked, &fields, &emojis, &created, &fetched, &a.Suspended, &a.FeaturedURI, &aka, &a.MovedTo, &attr, &a.Discoverable, &a.HideCollections, &a.Indexable, &a.NoIndex, &a.Limited, &a.ForceSensitive, &a.DomainLimited)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(fields), &a.Fields)
	json.Unmarshal([]byte(emojis), &a.Emojis)
	json.Unmarshal([]byte(aka), &a.AlsoKnownAs)
	json.Unmarshal([]byte(attr), &a.AttributionDomains)
	a.CreatedAt = time.Unix(created, 0)
	a.FetchedAt = time.Unix(fetched, 0)
	return a, nil
}

func (app *App) accountWhere(where string, args ...any) (*Account, error) {
	return scanAccount(app.db.QueryRow(`SELECT `+accountCols+` FROM accounts a WHERE `+where+` LIMIT 1`, args...))
}

func (app *App) AccountByID(id string) (*Account, error) { return app.accountWhere(`a.id=?`, id) }
func (app *App) AccountByURI(uri string) (*Account, error) {
	return app.accountWhere(`a.uri=?`, uri)
}

func (app *App) LocalAccount(username string) (*Account, error) {
	return app.accountWhere(`a.domain='' AND a.username=? COLLATE NOCASE AND EXISTS(SELECT 1 FROM users u WHERE u.account_id=a.id)`, username)
}

func (app *App) AccountByAcct(username, domain string) (*Account, error) {
	if domain == "" || domain == app.cfg.Host {
		return app.LocalAccount(username)
	}
	return app.accountWhere(`a.username=? COLLATE NOCASE AND a.domain=? ORDER BY a.fetched_at DESC`, username, strings.ToLower(domain))
}

func (app *App) accountsQuery(q string, args ...any) ([]*Account, error) {
	rows, err := app.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (app *App) saveAccount(a *Account) error {
	fields, _ := json.Marshal(a.Fields)
	emojis, _ := json.Marshal(a.Emojis)
	if a.Fields == nil {
		fields = []byte("[]")
	}
	if a.Emojis == nil {
		emojis = []byte("[]")
	}
	aka, _ := json.Marshal(a.AlsoKnownAs)
	if a.AlsoKnownAs == nil {
		aka = []byte("[]")
	}
	attr, _ := json.Marshal(a.AttributionDomains)
	if a.AttributionDomains == nil {
		attr = []byte("[]")
	}
	if a.ID == "" {
		a.ID = ulid.New()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	_, err := app.db.Exec(`INSERT INTO accounts (id,username,domain,uri,url,display_name,note,note_source,avatar,header,
		inbox,shared_inbox,outbox,followers_uri,following_uri,public_key_id,public_key,private_key,actor_type,locked,fields,emojis,created_at,fetched_at,suspended,featured_uri,also_known_as,moved_to,attribution_domains,discoverable,hide_collections,indexable,noindex,limited)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET username=excluded.username, url=excluded.url, display_name=excluded.display_name,
		note=excluded.note, note_source=excluded.note_source, avatar=excluded.avatar, header=excluded.header, inbox=excluded.inbox,
		shared_inbox=excluded.shared_inbox, outbox=excluded.outbox, followers_uri=excluded.followers_uri,
		following_uri=excluded.following_uri, public_key_id=excluded.public_key_id, public_key=excluded.public_key,
		actor_type=excluded.actor_type, locked=excluded.locked, fields=excluded.fields, emojis=excluded.emojis,
		fetched_at=excluded.fetched_at, suspended=MAX(accounts.suspended, excluded.suspended), featured_uri=excluded.featured_uri,
		also_known_as=excluded.also_known_as, moved_to=excluded.moved_to, attribution_domains=excluded.attribution_domains, discoverable=excluded.discoverable,
		hide_collections=excluded.hide_collections, indexable=excluded.indexable, noindex=excluded.noindex, limited=MAX(accounts.limited, excluded.limited)`,
		a.ID, a.Username, a.Domain, a.URI, a.URL, a.DisplayName, a.Note, a.NoteSource, a.Avatar, a.Header,
		a.Inbox, a.SharedInbox, a.Outbox, a.FollowersURI, a.FollowingURI, a.PublicKeyID, a.PublicKey, a.PrivateKey,
		a.ActorType, a.Locked, string(fields), string(emojis), a.CreatedAt.Unix(), a.FetchedAt.Unix(), a.Suspended,
		a.FeaturedURI, string(aka), a.MovedTo, string(attr), a.Discoverable, a.HideCollections, a.Indexable, a.NoIndex, a.Limited)
	return err
}

type User struct {
	Account  *Account
	Admin    bool
	LastSeen time.Time
}

func (app *App) ListUsers() ([]*User, error) {
	rows, err := app.db.Query(`SELECT ` + accountCols + `, u.admin, u.last_seen FROM accounts a JOIN users u ON u.account_id=a.id ORDER BY a.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		a := &Account{}
		u := &User{Account: a}
		var fields, emojis, aka, attr string
		var created, fetched, seen int64
		if err := rows.Scan(&a.ID, &a.Username, &a.Domain, &a.URI, &a.URL, &a.DisplayName, &a.Note, &a.NoteSource, &a.Avatar, &a.Header,
			&a.Inbox, &a.SharedInbox, &a.Outbox, &a.FollowersURI, &a.FollowingURI, &a.PublicKeyID, &a.PublicKey, &a.PrivateKey,
			&a.ActorType, &a.Locked, &fields, &emojis, &created, &fetched, &a.Suspended, &a.FeaturedURI, &aka, &a.MovedTo, &attr, &a.Discoverable, &a.HideCollections, &a.Indexable, &a.NoIndex, &a.Limited, &a.ForceSensitive, &a.DomainLimited, &u.Admin, &seen); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(created, 0)
		u.LastSeen = time.Unix(seen, 0)
		out = append(out, u)
	}
	return out, rows.Err()
}

func (app *App) isAdmin(accountID string) bool {
	var admin bool
	app.db.QueryRow(`SELECT admin FROM users WHERE account_id=?`, accountID).Scan(&admin)
	return admin
}

// ---- statuses ----

type Attachment struct {
	ID, StatusID, AccountID string
	URL, MediaType          string
	Description, File       string
	Width, Height           int
	PreviewURL, PreviewFile string // poster image of a video or cover of an audio file
	// FocusX, FocusY is the focal point Mastodon-style: -1..1, x to the right, y up; 0,0 is the center.
	FocusX, FocusY float64
}

// HasFocus reports whether a focal point other than the center is set.
func (at *Attachment) HasFocus() bool { return at.FocusX != 0 || at.FocusY != 0 }

// FocusData is the focal point for the web UI's data-focus attribute
// (app.js turns it into object-position; the CSP allows no inline styles).
func (at *Attachment) FocusData() string {
	if !at.HasFocus() {
		return ""
	}
	return strconv.FormatFloat(at.FocusX, 'f', 2, 64) + "," + strconv.FormatFloat(at.FocusY, 'f', 2, 64)
}

// parseFocus reads a focal point "x,y" (each -1..1, clamped).
func parseFocus(s string) (float64, float64, bool) {
	xs, ys, ok := strings.Cut(strings.TrimSpace(s), ",")
	if !ok {
		return 0, 0, false
	}
	x, err1 := strconv.ParseFloat(strings.TrimSpace(xs), 64)
	y, err2 := strconv.ParseFloat(strings.TrimSpace(ys), 64)
	if err1 != nil || err2 != nil || math.IsNaN(x) || math.IsNaN(y) {
		return 0, 0, false
	}
	return max(-1, min(1, x)), max(-1, min(1, y)), true
}

// setFocus stores an attachment's focal point from a "x,y" parameter.
func (app *App) setFocus(at *Attachment, s string) bool {
	x, y, ok := parseFocus(s)
	if !ok {
		return false
	}
	at.FocusX, at.FocusY = x, y
	app.db.Exec(`UPDATE attachments SET focus_x=?, focus_y=? WHERE id=?`, x, y, at.ID)
	return true
}

func (at *Attachment) Kind() string {
	switch {
	case strings.HasPrefix(at.MediaType, "image/"):
		return "image"
	case strings.HasPrefix(at.MediaType, "video/"):
		return "video"
	case strings.HasPrefix(at.MediaType, "audio/"):
		return "audio"
	}
	return "unknown"
}

type PollOption struct {
	Name  string `json:"name"`
	Votes int    `json:"votes"`
}

type Poll struct {
	Multiple bool         `json:"multiple"`
	EndTime  time.Time    `json:"end_time"`
	Options  []PollOption `json:"options"`
}

type Status struct {
	ID, URI, URL, AccountID   string
	Local                     bool
	InReplyToURI, InReplyToID string
	InReplyToAccountID        string
	BoostOfID                 string
	Content, Source, Spoiler  string
	Sensitive                 bool
	Visibility, Language      string
	Emojis                    []Emoji
	Poll                      *Poll
	CreatedAt, EditedAt       time.Time
	PinnedAt                  time.Time
	QuoteURI, CardURL         string
	QuoteState                string // "" (accepted or no quote), pending, rejected or revoked (FEP-044f)
	QuoteAuth                 string // QuoteAuthorization IRI approving the quote
	QuotePolicy               string // who may quote this post: public, followers, nobody, "" unknown

	// hydrated
	Translated       template.HTML // web UI: an English translation to show instead
	Account          *Account
	BoostOf          *Status
	InReplyToAccount *Account
	Attachments      []*Attachment
	Mentions         []*Account
	Tags             []string
	Liked, Boosted   bool
	Likes, Boosts    int
	Replies          int
	Depth            int
	Bookmarked       bool
	Quote            *Status         // quoted post, if known and visible
	Card             *LinkCard       // link preview
	Reactions        []ReactionCount // emoji reactions (Misskey, Pleroma, ...)
	FilteredBy       string          // matching keyword filter ("warn")
	Filter           *Filter         // and the keyword itself
	Blurred          bool            // media hidden by a "blur" filter
	MyVotes          map[int]bool    // poll choices the viewer voted for
}

type ReactionCount struct {
	Emoji, URL string
	Count      int
	Mine       bool
}

type LinkCard struct {
	URL, Title, Description, Image, Site string
}

func (s *Status) IsBoost() bool { return s.BoostOfID != "" }

// Path is the page of the status in our web interface.
func (s *Status) Path() string { return "/s/" + s.ID }

const statusCols = `s.id,s.uri,s.url,s.account_id,s.local,s.in_reply_to_uri,s.in_reply_to_id,s.in_reply_to_account_id,s.boost_of_id,
s.content,s.source,s.spoiler,(s.sensitive OR EXISTS(SELECT 1 FROM accounts fs WHERE fs.id=s.account_id AND fs.force_sensitive=1)),s.visibility,s.language,s.emojis,s.poll,s.created_at,s.edited_at,
s.pinned_at,s.quote_uri,s.card_url,s.quote_state,s.quote_auth,s.quote_policy`

func scanStatus(r scanner) (*Status, error) {
	s := &Status{}
	var emojis, poll string
	var created, edited, pinned int64
	err := r.Scan(&s.ID, &s.URI, &s.URL, &s.AccountID, &s.Local, &s.InReplyToURI, &s.InReplyToID, &s.InReplyToAccountID, &s.BoostOfID,
		&s.Content, &s.Source, &s.Spoiler, &s.Sensitive, &s.Visibility, &s.Language, &emojis, &poll, &created, &edited,
		&pinned, &s.QuoteURI, &s.CardURL, &s.QuoteState, &s.QuoteAuth, &s.QuotePolicy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(emojis), &s.Emojis)
	if poll != "" {
		s.Poll = &Poll{}
		json.Unmarshal([]byte(poll), s.Poll)
	}
	s.CreatedAt = time.Unix(created, 0)
	if edited > 0 {
		s.EditedAt = time.Unix(edited, 0)
	}
	if pinned > 0 {
		s.PinnedAt = time.Unix(pinned, 0)
	}
	return s, nil
}

func (app *App) statusWhere(where string, args ...any) (*Status, error) {
	return scanStatus(app.db.QueryRow(`SELECT `+statusCols+` FROM statuses s WHERE `+where+` LIMIT 1`, args...))
}

func (app *App) StatusByID(id string) (*Status, error)   { return app.statusWhere(`s.id=?`, id) }
func (app *App) StatusByURI(uri string) (*Status, error) { return app.statusWhere(`s.uri=?`, uri) }

func (app *App) insertStatus(s *Status) error {
	emojis, _ := json.Marshal(s.Emojis)
	if s.Emojis == nil {
		emojis = []byte("[]")
	}
	poll := ""
	if s.Poll != nil {
		b, _ := json.Marshal(s.Poll)
		poll = string(b)
	}
	var edited int64
	if !s.EditedAt.IsZero() {
		edited = s.EditedAt.Unix()
	}
	_, err := app.db.Exec(`INSERT INTO statuses (id,uri,url,account_id,local,in_reply_to_uri,in_reply_to_id,in_reply_to_account_id,boost_of_id,
		content,source,spoiler,sensitive,visibility,language,emojis,poll,created_at,edited_at,quote_uri,card_url,
		quote_state,quote_auth,quote_policy) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.URI, s.URL, s.AccountID, s.Local, s.InReplyToURI, s.InReplyToID, s.InReplyToAccountID, s.BoostOfID,
		s.Content, s.Source, s.Spoiler, s.Sensitive, s.Visibility, s.Language, string(emojis), poll, s.CreatedAt.Unix(), edited,
		s.QuoteURI, s.CardURL, s.QuoteState, s.QuoteAuth, s.QuotePolicy)
	return err
}

func (app *App) updateStatusContent(s *Status) error {
	// keep the previous version for the edit history
	var oldContent, oldSpoiler string
	var oldSensitive bool
	var created, edited int64
	if app.db.QueryRow(`SELECT content, spoiler, sensitive, created_at, edited_at FROM statuses WHERE id=?`, s.ID).
		Scan(&oldContent, &oldSpoiler, &oldSensitive, &created, &edited) == nil &&
		(oldContent != s.Content || oldSpoiler != s.Spoiler || oldSensitive != s.Sensitive) {
		at := created
		if edited > 0 {
			at = edited
		}
		app.db.Exec(`INSERT INTO status_edits (id, status_id, content, spoiler, sensitive, created_at) VALUES (?,?,?,?,?,?)`,
			ulid.New(), s.ID, oldContent, oldSpoiler, oldSensitive, at)
	}
	emojis, _ := json.Marshal(s.Emojis)
	if s.Emojis == nil {
		emojis = []byte("[]")
	}
	poll := ""
	if s.Poll != nil {
		b, _ := json.Marshal(s.Poll)
		poll = string(b)
	}
	_, err := app.db.Exec(`UPDATE statuses SET content=?, source=?, spoiler=?, sensitive=?, emojis=?, poll=?, edited_at=?, quote_uri=?, card_url=?,
		quote_state=?, quote_auth=?, quote_policy=? WHERE id=?`,
		s.Content, s.Source, s.Spoiler, s.Sensitive, string(emojis), poll, time.Now().Unix(), s.QuoteURI, s.CardURL,
		s.QuoteState, s.QuoteAuth, s.QuotePolicy, s.ID)
	return err
}

func (app *App) attachmentsFor(statusIDs []string) (map[string][]*Attachment, error) {
	out := map[string][]*Attachment{}
	if len(statusIDs) == 0 {
		return out, nil
	}
	q, args := inClause(`SELECT id,status_id,account_id,url,media_type,description,file,width,height,preview_url,preview_file,focus_x,focus_y FROM attachments WHERE status_id IN (%s) ORDER BY position`, statusIDs)
	rows, err := app.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		at := &Attachment{}
		var sid sql.NullString
		if err := rows.Scan(&at.ID, &sid, &at.AccountID, &at.URL, &at.MediaType, &at.Description, &at.File, &at.Width, &at.Height, &at.PreviewURL, &at.PreviewFile, &at.FocusX, &at.FocusY); err != nil {
			return nil, err
		}
		at.StatusID = sid.String
		out[at.StatusID] = append(out[at.StatusID], at)
	}
	return out, rows.Err()
}

func inClause(q string, ids []string) (string, []any) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.Replace(q, "%s", ph, 1), args
}

// ---- relationships ----

type Relationship struct {
	Following, Requested, FollowedBy, RequestedBy, Blocking, BlockedBy, Muting bool
	Endorsed                                                                   bool // featured on my profile
	DomainBlocking                                                             bool // I blocked their server (set by the web UI)
	MutingNotifications                                                        bool
	ShowingReblogs, Notifying                                                  bool      // options of my follow
	MuteExpiresAt                                                              time.Time // zero: muted indefinitely
}

func (app *App) relationship(me, other string) Relationship {
	var r Relationship
	var acc sql.NullBool
	r.ShowingReblogs = true
	if err := app.db.QueryRow(`SELECT accepted, show_reblogs, notify FROM follows WHERE account_id=? AND target_id=?`, me, other).Scan(&acc, &r.ShowingReblogs, &r.Notifying); err == nil {
		r.Following = acc.Bool
		r.Requested = !acc.Bool
	}
	if err := app.db.QueryRow(`SELECT accepted FROM follows WHERE account_id=? AND target_id=?`, other, me).Scan(&acc); err == nil {
		r.FollowedBy = acc.Bool
		r.RequestedBy = !acc.Bool
	}
	var x int
	r.Blocking = app.db.QueryRow(`SELECT 1 FROM blocks WHERE account_id=? AND target_id=?`, me, other).Scan(&x) == nil
	r.BlockedBy = app.db.QueryRow(`SELECT 1 FROM blocks WHERE account_id=? AND target_id=?`, other, me).Scan(&x) == nil
	var notif bool
	var expires int64
	if app.db.QueryRow(`SELECT notifications, expires_at FROM mutes WHERE account_id=? AND target_id=?`, me, other).Scan(&notif, &expires) == nil {
		r.Muting, r.MutingNotifications = true, notif
		if expires > 0 {
			r.MuteExpiresAt = time.Unix(expires, 0)
		}
	}
	r.Endorsed = app.db.QueryRow(`SELECT 1 FROM account_pins WHERE account_id=? AND target_id=?`, me, other).Scan(&x) == nil
	return r
}

func (app *App) isBlockedBy(localID, otherID string) bool {
	var x int
	return app.db.QueryRow(`SELECT 1 FROM blocks WHERE account_id=? AND target_id=?`, localID, otherID).Scan(&x) == nil
}

func (app *App) domainBlocked(domain string) bool {
	domain = strings.ToLower(domain)
	for domain != "" {
		var x int
		if app.db.QueryRow(`SELECT 1 FROM domain_blocks WHERE domain=?`, domain).Scan(&x) == nil {
			return true
		}
		i := strings.IndexByte(domain, '.')
		if i < 0 {
			break
		}
		domain = domain[i+1:]
	}
	return false
}

func (app *App) countInt(q string, args ...any) int {
	var n int
	app.db.QueryRow(q, args...).Scan(&n)
	return n
}

// notify records a notification for a local account, unless the recipient
// blocks the sender or it is a self-interaction.
func (app *App) notify(recipientID, typ, fromID, statusID string) {
	if recipientID == fromID || app.isBlockedBy(recipientID, fromID) {
		return
	}
	rec, err := app.AccountByID(recipientID)
	if err != nil || !rec.IsLocal() {
		return
	}
	// de-duplicate (e.g. repeated deliveries)
	var x int
	if app.db.QueryRow(`SELECT 1 FROM notifications WHERE account_id=? AND type=? AND from_account_id=? AND status_id=?`,
		recipientID, typ, fromID, statusID).Scan(&x) == nil {
		return
	}
	from, err := app.AccountByID(fromID)
	if err != nil {
		return
	}
	if typ != "poll" && app.conversationMuted(rec, statusID) {
		return
	}
	decision := app.notificationDecision(recipientID, typ, from, statusID)
	if decision == "drop" {
		return
	}
	id := ulid.New()
	filtered := decision == "filter"
	if _, err := app.db.Exec(`INSERT INTO notifications (id,account_id,type,from_account_id,status_id,created_at,filtered) VALUES (?,?,?,?,?,?,?)`,
		id, recipientID, typ, fromID, statusID, time.Now().Unix(), filtered); err == nil && !filtered {
		app.publishNotification(recipientID, id)
		app.pushNotification(recipientID, id)
	}
}
