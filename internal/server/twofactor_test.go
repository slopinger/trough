package server

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTOTPVectors(t *testing.T) {
	// RFC 6238 appendix B (SHA-1), last six digits
	secret := []byte("12345678901234567890")
	for ts, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		if got := totpCode(secret, ts/totpStep); got != want {
			t.Errorf("t=%d: %s, want %s", ts, got, want)
		}
	}
	s := b32.EncodeToString(secret)
	if !totpValid(s, totpCode(secret, time.Now().Unix()/totpStep-1), time.Now()) || totpValid(s, "000000x", time.Now()) {
		t.Error("totpValid")
	}
}

func TestTwoFactorLogin(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	secret := newTOTPSecret()
	la.db.Exec(`UPDATE users SET totp_secret=? WHERE account_id=?`, secret, bob.ID)
	codes := la.newRecoveryCodes(bob.ID)
	raw, _ := b32.DecodeString(secret)

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	post := func(path string, form url.Values) (int, string) {
		req, _ := http.NewRequest("POST", la.base+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	loggedIn := func() bool {
		resp, _ := c.Get(la.base + "/settings")
		resp.Body.Close()
		return resp.Request.URL.Path == "/settings"
	}
	if _, page := post("/login", url.Values{"username": {"bob"}, "password": {"password123"}}); !strings.Contains(page, `action="/login/code"`) || loggedIn() {
		t.Fatal("password alone logged in")
	}
	if code, _ := post("/login/code", url.Values{"code": {"123456"}}); code != 401 || loggedIn() {
		t.Fatalf("wrong code: %d", code)
	}
	post("/login/code", url.Values{"code": {totpCode(raw, time.Now().Unix()/totpStep)}})
	if !loggedIn() {
		t.Fatal("right code didn't log in")
	}

	// a recovery code works once
	jar2, _ := cookiejar.New(nil)
	c.Jar = jar2
	post("/login", url.Values{"username": {"bob"}, "password": {"password123"}})
	post("/login/code", url.Values{"code": {codes[0]}})
	if !loggedIn() {
		t.Fatal("recovery code didn't log in")
	}
	if la.checkSecondFactor(bob.ID, codes[0]) {
		t.Fatal("recovery code worked twice")
	}

	// apps can't skip the second factor with the password grant
	app := (&apiClient{t: t, base: la.base}).ok("POST", "/api/v1/apps", map[string]any{"client_name": "x", "redirect_uris": "urn:ietf:wg:oauth:2.0:oob"}).(map[string]any)
	code, _, _ := (&apiClient{t: t, base: la.base}).do("POST", "/oauth/token", map[string]any{"grant_type": "password", "username": "bob", "password": "password123",
		"client_id": app["client_id"], "client_secret": app["client_secret"]})
	if code != 400 {
		t.Fatalf("password grant with 2FA: %d", code)
	}
}
