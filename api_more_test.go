package main

import (
	"net/url"
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
