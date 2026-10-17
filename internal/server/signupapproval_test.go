package server

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

func TestSignupApproval(t *testing.T) {
	la := startLiveApp(t)
	boss, _ := la.CreateLocalUser("boss", "password123", true)
	la.setSetting("registrations", "approval")
	post := func(c *http.Client, path string, form url.Values) (int, string) {
		t.Helper()
		if c == nil {
			jar, _ := cookiejar.New(nil)
			c = &http.Client{Jar: jar}
		}
		req, _ := http.NewRequest("POST", la.base+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		req.Header.Set("X-Forwarded-For", form.Get("username")+".example") // distinct rate-limit keys
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	get := func(c *http.Client, path string) string {
		t.Helper()
		resp, err := c.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	apply := func(user, reason, invite string) (int, string) {
		return post(nil, "/signup", url.Values{"username": {user}, "password": {"password123"}, "confirm": {"password123"},
			"rules": {"1"}, "reason": {reason}, "invite": {invite}})
	}

	if form := get(http.DefaultClient, "/signup"); !strings.Contains(form, `name="reason"`) {
		t.Fatal("the sign-up form doesn't ask why")
	}
	if code, body := apply("eve", "I like pigs <3", ""); code != 200 || !strings.Contains(body, "The admins will look at your request") {
		t.Fatalf("applying: %d", code)
	}
	apply("frank", "", "")
	eve, err := la.accountWhere(`a.domain='' AND a.username='eve'`)
	if err != nil || !eve.Suspended || !la.signupPending(eve.ID) {
		t.Fatalf("eve isn't waiting: %v %+v", err, eve)
	}
	if code, body := post(nil, "/login", url.Values{"username": {"eve"}, "password": {"password123"}}); code != 403 || !strings.Contains(body, "waiting for approval") {
		t.Errorf("waiting account logged in: %d", code)
	}
	if err := la.UnsuspendAccount(eve); err == nil {
		t.Error("lifting the suspension skipped the approval")
	}
	api := &apiClient{t: t, base: la.base}
	if inst := api.ok("GET", "/api/v2/instance", nil).(map[string]any); inst["registrations"].(map[string]any)["approval_required"] != true {
		t.Error("instance doesn't say sign-ups need approval")
	}

	// admins hear about it and decide on /admin
	web := webClient(t, la, "boss", "password123")
	if n := get(web, "/notifications"); !strings.Contains(n, "asked to join") {
		t.Error("no notification about the applicant")
	}
	page := get(web, "/admin")
	if !strings.Contains(page, `id="signups"`) || !strings.Contains(page, "I like pigs &lt;3") || strings.Contains(page, `action="/admin/accounts/`+eve.ID+`/unsuspend"`) {
		t.Fatal("admin page doesn't list the applicants on their own")
	}
	csrf := strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0]
	frank, _ := la.accountWhere(`a.domain='' AND a.username='frank'`)
	post(web, "/admin/signups/"+eve.ID+"/approve", url.Values{"csrf": {csrf}})
	post(web, "/admin/signups/"+frank.ID+"/reject", url.Values{"csrf": {csrf}})
	webClient(t, la, "eve", "password123") // fails the test unless eve can log in
	if _, err := la.AccountByID(frank.ID); err == nil {
		t.Error("the rejected account still exists")
	}
	if _, err := la.CreateLocalUser("frank", "password123", false); err != nil {
		t.Errorf("the rejected name isn't free: %v", err)
	}

	// an invite link skips the approval
	invite, _ := la.CreateInvite(boss, 1, 0)
	if code, body := apply("zed", "", invite); code != 200 || !strings.Contains(body, "Welcome") {
		t.Errorf("invited sign-up: %d", code)
	}
}
