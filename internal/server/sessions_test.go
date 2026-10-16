package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestBrowserName(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0":                                                                  "Firefox on Linux",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0":           "Edge on Windows",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1": "Safari on iOS",
		"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36":                            "Chrome on Android",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15":                      "Safari on macOS",
		"curl/8.0": "Unknown browser",
	} {
		if got := browserName(ua); got != want {
			t.Errorf("%q = %q, want %q", ua, got, want)
		}
	}
}

func TestWebSessions(t *testing.T) {
	la := startLiveApp(t)
	la.CreateLocalUser("bob", "password123", false)
	cat, _ := la.CreateLocalUser("cat", "password123", false)
	here, there := webClient(t, la, "bob", "password123"), webClient(t, la, "bob", "password123")
	webClient(t, la, "cat", "password123")
	get := func(c *http.Client) (string, string) {
		t.Helper()
		resp, err := c.Get(la.base + "/settings")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.Request.URL.Path
	}
	post := func(c *http.Client, path, csrf string) {
		t.Helper()
		req, _ := http.NewRequest("POST", la.base+path, strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	page, _ := get(here)
	if strings.Count(page, "/settings/sessions/") != 2 || !strings.Contains(page, "This browser") {
		t.Fatal("settings don't list both browsers")
	}
	csrf := strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0]

	// someone else's session can't be logged out from here
	var catSession string
	la.db.QueryRow(`SELECT id FROM sessions WHERE account_id=?`, cat.ID).Scan(&catSession)
	post(here, "/settings/sessions/"+catSession+"/revoke", csrf)
	if la.countInt(`SELECT COUNT(*) FROM sessions WHERE id=?`, catSession) != 1 {
		t.Error("logged out another account's browser")
	}

	post(here, "/settings/sessions/others/revoke", csrf)
	if _, path := get(there); path != "/login" {
		t.Errorf("other browser still logged in (%s)", path)
	}
	if page, path := get(here); path != "/settings" || strings.Contains(page, "Log out everywhere else") {
		t.Error("this browser was logged out too, or the list didn't shrink")
	}
}
