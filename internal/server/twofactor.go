package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// Two-factor authentication with TOTP (RFC 6238: 30-second steps, six
// digits, SHA-1, as authenticator apps expect). With it on, logging in on
// the web (which apps use through OAuth) asks for a code after the
// password, and the OAuth password grant is refused, as on Mastodon.
// Ten one-time recovery codes replace the app if it gets lost.

const (
	totpStep        = 30
	recoveryCodes   = 10
	pendingLoginTTL = 5 * time.Minute
	twoFactorCookie = "trough_2fa"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	h := hmac.New(sha1.New, secret)
	h.Write(msg[:])
	sum := h.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1000000)
}

// totpValid accepts the current code and the ones next to it (clock drift).
func totpValid(secretB32, code string, now time.Time) bool {
	secret, err := b32.DecodeString(strings.ToUpper(secretB32))
	if err != nil || len(code) != 6 {
		return false
	}
	step := now.Unix() / totpStep
	ok := 0
	for d := int64(-1); d <= 1; d++ {
		ok |= subtle.ConstantTimeCompare([]byte(totpCode(secret, step+d)), []byte(code))
	}
	return ok == 1
}

func newTOTPSecret() string {
	b := make([]byte, 20)
	rand.Read(b)
	return b32.EncodeToString(b)
}

func (app *App) twoFactorEnabled(accountID string) bool {
	var s string
	app.db.QueryRow(`SELECT totp_secret FROM users WHERE account_id=?`, accountID).Scan(&s)
	return s != ""
}

func (app *App) otpauthURI(acc *Account, secret string) string {
	label := url.PathEscape(app.cfg.Host + ":" + acc.Username)
	return "otpauth://totp/" + label + "?secret=" + secret + "&issuer=" + url.QueryEscape(app.cfg.Host) + "&algorithm=SHA1&digits=6&period=30"
}

// checkSecondFactor accepts a TOTP code or an unused recovery code (which is then used up).
func (app *App) checkSecondFactor(accountID, code string) bool {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	var secret, recovery string
	if app.db.QueryRow(`SELECT totp_secret, recovery_codes FROM users WHERE account_id=?`, accountID).Scan(&secret, &recovery) != nil || secret == "" {
		return false
	}
	if totpValid(secret, code, time.Now()) {
		return true
	}
	var hashes []string
	json.Unmarshal([]byte(recovery), &hashes)
	h := sha256Hex(strings.ToLower(code))
	for i, rc := range hashes {
		if subtle.ConstantTimeCompare([]byte(rc), []byte(h)) == 1 {
			hashes = append(hashes[:i], hashes[i+1:]...)
			b, _ := json.Marshal(hashes)
			app.db.Exec(`UPDATE users SET recovery_codes=? WHERE account_id=?`, string(b), accountID)
			return true
		}
	}
	return false
}

// newRecoveryCodes stores fresh recovery codes (hashed) and returns them.
func (app *App) newRecoveryCodes(accountID string) []string {
	var codes, hashes []string
	for range recoveryCodes {
		c := randomToken(5) // 10 hex digits
		codes = append(codes, c)
		hashes = append(hashes, sha256Hex(c))
	}
	b, _ := json.Marshal(hashes)
	app.db.Exec(`UPDATE users SET recovery_codes=? WHERE account_id=?`, string(b), accountID)
	return codes
}

// Logins waiting for their second factor, by a random token in a cookie.
type pendingLogin struct {
	accountID, next string
	expires         time.Time
}

var (
	pendingMu     sync.Mutex
	pendingLogins = map[string]pendingLogin{}
)

func startPendingLogin(w http.ResponseWriter, secure bool, accountID, next string) {
	token := randomToken(24)
	pendingMu.Lock()
	for k, v := range pendingLogins {
		if time.Now().After(v.expires) {
			delete(pendingLogins, k)
		}
	}
	pendingLogins[sha256Hex(token)] = pendingLogin{accountID, next, time.Now().Add(pendingLoginTTL)}
	pendingMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: twoFactorCookie, Value: token, Path: "/login", HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteStrictMode, MaxAge: int(pendingLoginTTL.Seconds())})
}

func pendingLoginFor(r *http.Request) (string, pendingLogin, bool) {
	c, err := r.Cookie(twoFactorCookie)
	if err != nil {
		return "", pendingLogin{}, false
	}
	key := sha256Hex(c.Value)
	pendingMu.Lock()
	defer pendingMu.Unlock()
	pl, ok := pendingLogins[key]
	if !ok || time.Now().After(pl.expires) {
		return "", pendingLogin{}, false
	}
	return key, pl, true
}

func (app *App) handleLoginCode(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title = "Log in"
	key, pl, ok := pendingLoginFor(r)
	if !ok {
		p.Error = "That login took too long. Please log in again."
		app.renderStatus(w, http.StatusUnauthorized, "login", p)
		return
	}
	ip := clientIP(r)
	if app.logins.blocked(ip) {
		p.Error = "Too many failed attempts. Try again in a few minutes."
		app.renderStatus(w, http.StatusTooManyRequests, "login_code", p)
		return
	}
	if !app.checkSecondFactor(pl.accountID, r.FormValue("code")) {
		app.logins.hit(ip)
		p.Error = "That code is not right. Use the current code from your authenticator app, or a recovery code."
		app.renderStatus(w, http.StatusUnauthorized, "login_code", p)
		return
	}
	pendingMu.Lock()
	delete(pendingLogins, key)
	pendingMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: twoFactorCookie, Value: "", Path: "/login", MaxAge: -1})
	acc, err := app.AccountByID(pl.accountID)
	if err != nil || app.startSession(w, r, acc) != nil {
		http.Error(w, "could not create session", 500)
		return
	}
	http.Redirect(w, r, pl.next, http.StatusSeeOther)
}

// ---- settings ----

func (app *App) handleTwoFactorSetup(w http.ResponseWriter, r *http.Request, p *Page) {
	if app.twoFactorEnabled(p.Me.ID) {
		http.Redirect(w, r, "/settings#2fa", http.StatusSeeOther)
		return
	}
	var secret string
	app.db.QueryRow(`SELECT totp_pending FROM users WHERE account_id=?`, p.Me.ID).Scan(&secret)
	if secret == "" || r.Method == http.MethodPost && r.FormValue("code") == "" {
		secret = newTOTPSecret()
		app.db.Exec(`UPDATE users SET totp_pending=? WHERE account_id=?`, secret, p.Me.ID)
	}
	p.Title, p.Section = "Two-factor authentication", "settings"
	p.Query = secret
	if r.Method == http.MethodPost && r.FormValue("code") != "" {
		if !totpValid(secret, strings.TrimSpace(r.FormValue("code")), time.Now()) {
			p.Error = "That code doesn't match. Check that your phone's clock is right and try the current code."
		} else {
			app.db.Exec(`UPDATE users SET totp_secret=totp_pending, totp_pending='' WHERE account_id=?`, p.Me.ID)
			p.Flash = "Two-factor authentication is on. Keep these recovery codes somewhere safe; each works once if you lose your phone."
			p.Codes = app.newRecoveryCodes(p.Me.ID)
		}
	}
	app.render(w, "twofactor", p)
}

func (app *App) handleTwoFactorQR(w http.ResponseWriter, r *http.Request, p *Page) {
	var secret string
	app.db.QueryRow(`SELECT totp_pending FROM users WHERE account_id=?`, p.Me.ID).Scan(&secret)
	if secret == "" {
		http.NotFound(w, r)
		return
	}
	png, err := qrcode.Encode(app.otpauthURI(p.Me, secret), qrcode.Medium, 256)
	if err != nil {
		http.Error(w, "qr failed", 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

func (app *App) handleTwoFactorDisable(w http.ResponseWriter, r *http.Request, p *Page) {
	if _, err := app.checkPassword(p.Me.Username, r.FormValue("password")); err != nil {
		setFlash(w, "Wrong password; two-factor authentication stays on.")
	} else {
		app.db.Exec(`UPDATE users SET totp_secret='', totp_pending='', recovery_codes='[]' WHERE account_id=?`, p.Me.ID)
		setFlash(w, "Two-factor authentication is off.")
	}
	http.Redirect(w, r, "/settings#2fa", http.StatusSeeOther)
}
