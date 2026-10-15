package main

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Endpoints found missing by comparing with the open-source iOS clients
// (Ice Cubes and the official Mastodon app).

func TestAPIReport(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	id := newID()
	la.insertStatus(&Status{ID: id, URI: r.Base + "/s/bad", AccountID: alice.ID, Visibility: "public", Content: "<p>spam</p>", CreatedAt: parseTime("2026-01-01T00:00:00Z")})
	rep := c.ok("POST", "/api/v1/reports", url.Values{"account_id": {alice.ID}, "status_ids[]": {id, "nope"}, "comment": {"spam"},
		"forward": {"true"}, "category": {"spam"}}).(map[string]any)
	if rep["forwarded"] != true || rep["category"] != "spam" || len(rep["status_ids"].([]any)) != 1 || rep["target_account"].(map[string]any)["id"] != alice.ID {
		t.Fatalf("report = %v", rep)
	}
	eventually(t, "Flag delivered", func() bool { return len(r.receivedOf("Flag")) == 1 })
	if len(la.openReports()) != 1 {
		t.Error("report not stored")
	}
	if code, _, _ := c.do("POST", "/api/v1/reports", map[string]any{"account_id": bob.ID}); code != 422 {
		t.Errorf("reporting yourself: %d", code)
	}
	if code, _, _ := c.do("POST", "/api/v1/reports", map[string]any{"account_id": "missing"}); code != 404 {
		t.Errorf("unknown account: %d", code)
	}
}

func TestAPIDeleteProfileImages(t *testing.T) {
	la, r, bob, _ := featureFixture(t)
	token := oauthLogin(t, la, "bob", "password123", "read write")
	c := &apiClient{t: t, base: la.base, token: token}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range []string{"avatar", "header"} {
		fw, _ := mw.CreateFormFile(f, f+".png")
		fw.Write(pngBytes())
	}
	mw.Close()
	req, _ := http.NewRequest("PATCH", la.base+"/api/v1/accounts/update_credentials", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(err, resp.StatusCode)
	}
	resp.Body.Close()
	if me, _ := la.AccountByID(bob.ID); !strings.Contains(me.Avatar, "/media/") || !strings.Contains(me.Header, "/media/") {
		t.Fatalf("images not set: %q %q", me.Avatar, me.Header)
	}
	acc := c.ok("DELETE", "/api/v1/profile/avatar", nil).(map[string]any)
	if strings.Contains(acc["avatar"].(string), "/media/") {
		t.Errorf("avatar still %v", acc["avatar"])
	}
	c.ok("DELETE", "/api/v1/profile/header", nil)
	if me, _ := la.AccountByID(bob.ID); me.Avatar != "" || me.Header != "" {
		t.Errorf("images left: %q %q", me.Avatar, me.Header)
	}
	if code, _, _ := c.do("DELETE", "/api/v1/profile/banner", nil); code != 404 {
		t.Errorf("unknown image: %d", code)
	}
	eventually(t, "profile Update federated", func() bool { return len(r.receivedOf("Update")) >= 3 })
}

func TestAPIStatusQuotes(t *testing.T) {
	la, _, bob, alice := featureFixture(t)
	ctx := context.Background()
	eve, _ := la.CreateLocalUser("eve", "password123", false)
	orig, _ := la.PostStatus(ctx, bob, PostInput{Text: "quote me", Visibility: "public"})
	q1, _ := la.PostStatus(ctx, eve, PostInput{Text: "public quote", Visibility: "public", QuoteOf: orig.ID})
	la.PostStatus(ctx, eve, PostInput{Text: "private quote", Visibility: "private", QuoteOf: orig.ID})
	la.PostStatus(ctx, eve, PostInput{Text: "not a quote", Visibility: "public"})
	la.insertStatus(&Status{ID: newID(), URI: alice.URI + "/s/q", AccountID: alice.ID, Visibility: "public", Content: "<p>remote quote</p>",
		QuoteURI: orig.URI, CreatedAt: parseTime("2026-01-01T00:00:00Z")})
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read")}
	got := c.ok("GET", "/api/v1/statuses/"+orig.ID+"/quotes", nil).([]any)
	texts := []string{}
	for _, s := range got {
		texts = append(texts, plainText(s.(map[string]any)["content"].(string)))
	}
	if len(got) != 2 || got[1].(map[string]any)["id"] != q1.ID || !strings.HasPrefix(texts[0], "remote quote") {
		t.Fatalf("quotes = %q", texts)
	}
	if got := (&apiClient{t: t, base: la.base}).ok("GET", "/api/v1/statuses/"+orig.ID+"/quotes", nil).([]any); len(got) != 2 {
		t.Errorf("anonymous sees %d quotes", len(got))
	}
}

func TestAPILinkTimeline(t *testing.T) {
	la, _, bob, alice := featureFixture(t)
	link := "https://news.example/story"
	for i, vis := range []string{"public", "public", "private"} {
		la.insertStatus(&Status{ID: newID(), URI: alice.URI + "/s/" + string(rune('a'+i)), AccountID: alice.ID, Visibility: vis,
			Content: "<p>read this</p>", CardURL: link, CreatedAt: parseTime("2026-01-01T00:00:00Z")})
	}
	la.PostStatus(context.Background(), bob, PostInput{Text: "unrelated", Visibility: "public"})
	pub := &apiClient{t: t, base: la.base}
	if got := pub.ok("GET", "/api/v1/timelines/link?url="+url.QueryEscape(link), nil).([]any); len(got) != 2 {
		t.Errorf("link timeline has %d posts", len(got))
	}
	if code, _, _ := pub.do("GET", "/api/v1/timelines/link", nil); code != 422 {
		t.Errorf("no url: %d", code)
	}
}
