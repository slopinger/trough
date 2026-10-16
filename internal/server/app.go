package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"git.freifick.net/slop/trough/internal/httpsig"
	"git.freifick.net/slop/trough/internal/ulid"
)

type App struct {
	cfg         *Config
	db          *sql.DB
	client      *http.Client
	mediaClient *http.Client
	tmpl        *templates

	mediaDir, cacheDir string
	proxyKey           []byte
	proxyLocks         proxyFetchLocks

	instMu   sync.Mutex
	vapid    *ecdsa.PrivateKey
	instance *Account

	inboxQueue  chan inboxJob
	deliverKick chan struct{}
	logins      *rateLimiter
	interacts   *rateLimiter // remote follow lookups by visitors
	aiLimit     *rateLimiter
	aiBaseURL   string // chat completions base URL override (tests)
	aiHTTP      *http.Client
}

func NewApp(cfg *Config) (*App, error) {
	db, err := openDB(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	app := &App{
		cfg:         cfg,
		db:          db,
		mediaDir:    filepath.Join(cfg.DataDir, "media"),
		cacheDir:    filepath.Join(cfg.DataDir, "cache"),
		inboxQueue:  make(chan inboxJob, 1000),
		deliverKick: make(chan struct{}, 1),
		logins:      newRateLimiter(10, 15*time.Minute),
		interacts:   newRateLimiter(20, 15*time.Minute),
		aiLimit:     newRateLimiter(aiRequestsPerHour, time.Hour),
		aiHTTP:      &http.Client{Timeout: 4 * time.Minute},
	}
	app.client = app.newRemoteClient()
	app.mediaClient = app.newRemoteClient()
	app.mediaClient.Timeout = 2 * time.Minute
	for _, d := range []string{app.mediaDir, app.cacheDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	// stored host must match: the host is baked into every URI we hand out
	if h := app.getSetting("host"); h == "" {
		app.setSetting("host", cfg.Host)
	} else if h != cfg.Host {
		return nil, fmt.Errorf("database belongs to host %q, not %q", h, cfg.Host)
	}
	key := app.getSetting("proxy_key")
	if key == "" {
		b := make([]byte, 32)
		rand.Read(b)
		key = hex.EncodeToString(b)
		app.setSetting("proxy_key", key)
	}
	app.proxyKey, _ = hex.DecodeString(key)
	if _, err := app.ensureInstanceActor(); err != nil {
		return nil, err
	}
	app.tmpl, err = loadTemplates(app)
	if err != nil {
		return nil, err
	}
	return app, nil
}

func (app *App) Close() { app.db.Close() }

// Start launches the background workers.
func (app *App) Start(ctx context.Context) {
	for i := 0; i < 4; i++ {
		go app.inboxWorker(ctx)
	}
	go app.deliveryLoop(ctx)
	go app.housekeeping(ctx)
	go app.scheduleLoop(ctx)
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				app.closeEndedPolls()
				app.resendPendingFollows(now)
				app.expireMutes(now)
				if now.Minute()%10 == 0 {
					app.cleanupStatuses(now)
				}
			}
		}
	}()
}

func (app *App) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		app.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
		app.cleanOrphanMedia()
		app.cleanCache(7 * 24 * time.Hour)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// cleanOrphanMedia removes uploads that never got attached to a post.
func (app *App) cleanOrphanMedia() {
	cut := time.Now().Add(-24 * time.Hour).Unix()
	rows, err := app.db.Query(`SELECT id, file FROM attachments WHERE status_id IS NULL AND created_at < ?
		AND id NOT IN (SELECT j.value FROM scheduled_statuses ss, json_each(ss.media_ids) j)`, cut)
	if err != nil {
		return
	}
	type orphan struct{ id, file string }
	var list []orphan
	for rows.Next() {
		var o orphan
		rows.Scan(&o.id, &o.file)
		list = append(list, o)
	}
	rows.Close()
	for _, o := range list {
		app.removeMediaFiles(`id=?`, o.id)
		app.db.Exec(`DELETE FROM attachments WHERE id=?`, o.id)
	}
}

// ---- URIs of local things ----

func (app *App) actorURI(username string) string { return app.cfg.BaseURL() + "/users/" + username }
func (app *App) statusURI(username, id string) string {
	return app.actorURI(username) + "/statuses/" + id
}
func (app *App) statusURL(username, id string) string {
	return app.cfg.BaseURL() + "/@" + username + "/" + id
}

// ---- instance actor ----

// The instance actor signs GET requests (needed for servers running in
// "authorized fetch" mode) and is not a real user.
func (app *App) ensureInstanceActor() (*Account, error) {
	uri := app.cfg.BaseURL() + "/actor"
	acc, err := app.AccountByURI(uri)
	if err == nil {
		app.instance = acc
		return acc, nil
	}
	priv, pub, err := httpsig.GenerateKey()
	if err != nil {
		return nil, err
	}
	name := app.cfg.Host
	if h, _, ok := strings.Cut(name, ":"); ok {
		name = h
	}
	acc = &Account{
		Username:     name,
		URI:          uri,
		URL:          app.cfg.BaseURL() + "/about",
		Inbox:        app.cfg.BaseURL() + "/actor/inbox",
		SharedInbox:  app.cfg.BaseURL() + "/inbox",
		Outbox:       app.cfg.BaseURL() + "/actor/outbox",
		PublicKeyID:  uri + "#main-key",
		PublicKey:    pub,
		PrivateKey:   priv,
		ActorType:    "Application",
		Locked:       true,
		FollowersURI: "",
		FetchedAt:    time.Now(),
	}
	// the instance actor is local but has no users row; avoid clashing with
	// real usernames by keeping it out of the local-username unique index
	acc.Domain = ""
	if err := app.saveAccountInstance(acc); err != nil {
		return nil, err
	}
	app.instance = acc
	return acc, nil
}

func (app *App) saveAccountInstance(acc *Account) error {
	acc.ID = ulid.New()
	acc.CreatedAt = time.Now()
	return app.saveAccount(acc)
}

func (app *App) instanceActor() *Account {
	app.instMu.Lock()
	defer app.instMu.Unlock()
	return app.instance
}

// ---- local users ----

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_]{1,30}$`)

var reservedUsernames = map[string]bool{"actor": true, "admin": true, "root": true, "inbox": true, "about": true,
	"settings": true, "login": true, "logout": true, "api": true, "media": true, "static": true, "proxy": true,
	"instance": true, "nodeinfo": true, "well_known": true, "support": true, "abuse": true, "postmaster": true}

func (app *App) CreateLocalUser(username, password string, admin bool) (*Account, error) {
	if !usernameRe.MatchString(username) {
		return nil, errors.New("username must be 1-30 characters of a-z, 0-9 and _")
	}
	if reservedUsernames[strings.ToLower(username)] || strings.EqualFold(username, app.instanceActor().Username) {
		return nil, errors.New("username is reserved")
	}
	if len(password) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	if _, err := app.accountWhere(`a.domain='' AND a.username=? COLLATE NOCASE`, username); err == nil {
		return nil, errors.New("username is taken")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	priv, pub, err := httpsig.GenerateKey()
	if err != nil {
		return nil, err
	}
	uri := app.actorURI(username)
	acc := &Account{
		ID:           ulid.New(),
		Username:     username,
		Discoverable: true,
		URI:          uri,
		URL:          app.cfg.BaseURL() + "/@" + username,
		Inbox:        uri + "/inbox",
		SharedInbox:  app.cfg.BaseURL() + "/inbox",
		Outbox:       uri + "/outbox",
		FollowersURI: uri + "/followers",
		FollowingURI: uri + "/following",
		PublicKeyID:  uri + "#main-key",
		PublicKey:    pub,
		PrivateKey:   priv,
		ActorType:    "Person",
		FetchedAt:    time.Now(),
	}
	if err := app.saveAccount(acc); err != nil {
		return nil, err
	}
	if _, err := app.db.Exec(`INSERT INTO users (account_id,password_hash,admin,created_at) VALUES (?,?,?,?)`,
		acc.ID, string(hash), admin, time.Now().Unix()); err != nil {
		return nil, err
	}
	log.Printf("created local user @%s", username)
	return acc, nil
}

func (app *App) SetPassword(accountID, password string) error {
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = app.db.Exec(`UPDATE users SET password_hash=? WHERE account_id=?`, string(hash), accountID)
	if err == nil {
		// log out everywhere
		app.db.Exec(`DELETE FROM sessions WHERE account_id=?`, accountID)
	}
	return err
}

func (app *App) checkPassword(username, password string) (*Account, error) {
	acc, err := app.LocalAccount(username)
	if err != nil {
		// spend similar time to avoid user enumeration
		bcrypt.CompareHashAndPassword([]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5BfB3WbH2RSHfQn9RkQ2ItW8P1yIq"), []byte(password))
		return nil, errors.New("invalid username or password")
	}
	var hash string
	if err := app.db.QueryRow(`SELECT password_hash FROM users WHERE account_id=?`, acc.ID).Scan(&hash); err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, errors.New("invalid username or password")
	}
	return acc, nil
}

// ---- rate limiting ----

type rateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{max: max, window: window, hits: map[string][]time.Time{}}
}

func (rl *rateLimiter) blocked(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cut := time.Now().Add(-rl.window)
	h := rl.hits[key]
	for len(h) > 0 && h[0].Before(cut) {
		h = h[1:]
	}
	rl.hits[key] = h
	if len(h) == 0 {
		delete(rl.hits, key)
	}
	return len(h) >= rl.max
}

func (rl *rateLimiter) hit(key string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.hits[key] = append(rl.hits[key], time.Now())
}
