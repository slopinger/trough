package main

import (
	"bytes"
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
