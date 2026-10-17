package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestModerationWarnings(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	boss, _ := la.CreateLocalUser("boss", "password123", true)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	spam, _ := la.CreateLocalUser("spam", "password123", false)
	post, _ := la.PostStatus(ctx, spam, PostInput{Text: "cheap pills here", Visibility: "public"})

	// limiting from a report warns the account, with the admins' message and the post
	reportID, _ := la.FileReport(bob, spam, []string{post.ID}, "spam", false)
	if _, err := la.resolveReport(boss, reportID, "limit", "", "Please stop advertising."); err != nil {
		t.Fatal(err)
	}
	ns, _ := la.notifications(spam, "")
	if len(ns) != 1 || ns[0].Type != "moderation_warning" || ns[0].Warning == nil {
		t.Fatalf("notifications: %+v", ns)
	}
	w := ns[0].Warning
	if w.Action != "silence" || w.Text != "Please stop advertising." || len(w.Statuses) != 1 || w.Statuses[0].Text != "cheap pills here" {
		t.Fatalf("warning: %+v", w)
	}

	// clients see Mastodon's moderation_warning entity
	api := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "spam", "password123", "read write")}
	list := api.ok("GET", "/api/v1/notifications", nil).([]any)
	mw, _ := list[0].(map[string]any)["moderation_warning"].(map[string]any)
	if list[0].(map[string]any)["type"] != "moderation_warning" || mw["action"] != "silence" || mw["appeal"] != nil {
		t.Fatalf("api notification: %v", list[0])
	}
	grouped := api.ok("GET", "/api/v2/notifications", nil).(map[string]any)
	if g := grouped["notification_groups"].([]any)[0].(map[string]any); g["moderation_warning"] == nil {
		t.Errorf("grouped notification lacks the warning: %v", g)
	}

	// the account status page shows it and takes one appeal
	web := webClient(t, la, "spam", "password123")
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
	post2 := func(c *http.Client, page, action string, form url.Values) {
		t.Helper()
		body := get(c, page)
		form.Set("csrf", strings.SplitN(strings.SplitN(body, `name="csrf" value="`, 2)[1], `"`, 2)[0])
		req, _ := http.NewRequest("POST", la.base+action, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if p := get(web, "/disputes/strikes"); !strings.Contains(p, "Your account has been limited") {
		t.Error("account status doesn't list the warning")
	}
	page := get(web, "/disputes/strikes/"+w.ID)
	if !strings.Contains(page, "Please stop advertising.") || !strings.Contains(page, "cheap pills here") || !strings.Contains(page, "Send appeal") {
		t.Error("warning page lacks the message, post or appeal form")
	}
	if p := get(webClient(t, la, "bob", "password123"), "/disputes/strikes/"+w.ID); strings.Contains(p, "Please stop") {
		t.Error("bob can read spam's warning")
	}
	post2(web, "/disputes/strikes/"+w.ID, "/disputes/strikes/"+w.ID+"/appeal", url.Values{"text": {"It was a joke."}})
	if err := la.appeal(spam, w.ID, "again"); err == nil {
		t.Error("a second appeal was accepted")
	}

	// the admin approves it, which lifts the limit
	admin := webClient(t, la, "boss", "password123")
	if p := get(admin, "/admin/reports?tab=appeals"); !strings.Contains(p, "It was a joke.") || !strings.Contains(p, "Appeals (1)") {
		t.Fatal("appeals tab doesn't show the appeal")
	}
	post2(admin, "/admin/reports?tab=appeals", "/admin/appeals/"+w.ID+"/approve", url.Values{})
	if spam, _ = la.AccountByID(spam.ID); spam.Limited {
		t.Error("approving the appeal didn't lift the limit")
	}
	if p := get(web, "/disputes/strikes/"+w.ID); !strings.Contains(p, "approved your appeal") {
		t.Error("the warning page doesn't say the appeal was approved")
	}
	if a := api.ok("GET", "/api/v1/notifications", nil).([]any)[0].(map[string]any)["moderation_warning"].(map[string]any)["appeal"].(map[string]any); a["state"] != "approved" {
		t.Errorf("api appeal: %v", a)
	}

	// a plain warning, and none for "no action" or remote accounts
	r2, _ := la.FileReport(bob, spam, nil, "rude", false)
	la.resolveReport(boss, r2, "warn", "", "Be nice.")
	r3, _ := la.FileReport(bob, spam, nil, "again", false)
	la.resolveReport(boss, r3, "none", "", "ignored")
	if n := la.countInt(`SELECT COUNT(*) FROM account_warnings WHERE account_id=?`, spam.ID); n != 2 {
		t.Errorf("%d warnings, want 2", n)
	}
	r := newRemote(t, la)
	r.put("/users/alice", mastodonActor)
	alice, _ := la.getAccount(ctx, r.Base+"/users/alice")
	r4, _ := la.FileReport(bob, alice, nil, "remote", false)
	if _, err := la.resolveReport(boss, r4, "warn", "", "x"); err == nil {
		t.Error("warned a remote account")
	}
}
