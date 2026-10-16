package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestHideCollections(t *testing.T) {
	la := startLiveApp(t)
	ann, _ := la.CreateLocalUser("ann", "password123", false)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	cat, _ := la.CreateLocalUser("cat", "password123", false)
	cat.Discoverable = true
	la.saveAccount(cat)
	for _, f := range [][2]*Account{{ann, bob}, {bob, cat}} {
		if err := la.Follow(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if len(la.familiarFollowers(ann, cat, 10)) != 1 || len(la.suggestions(ann, 10)) == 0 {
		t.Fatal("setup: bob should be a familiar follower of cat and cat a suggestion for ann")
	}

	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	me := c.ok("PATCH", "/api/v1/accounts/update_credentials", url.Values{"hide_collections": {"true"}}).(map[string]any)
	if me["hide_collections"] != true {
		t.Fatalf("update_credentials: %v", me["hide_collections"])
	}
	anon := &apiClient{t: t, base: la.base}
	for _, path := range []string{"/following", "/followers"} {
		if l := anon.ok("GET", "/api/v1/accounts/"+bob.ID+path, nil).([]any); len(l) != 0 {
			t.Errorf("others see %s: %d", path, len(l))
		}
		if l := c.ok("GET", "/api/v1/accounts/"+bob.ID+path, nil).([]any); len(l) != 1 {
			t.Errorf("bob's own %s: %d", path, len(l))
		}
	}

	// Federation only gets the counts, like from Mastodon.
	fetch := func(path string) (int, string) {
		req, _ := http.NewRequest("GET", la.base+path, nil)
		req.Header.Set("Accept", "application/activity+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := fetch("/users/bob/following"); code != 200 || !strings.Contains(body, `"totalItems":1`) || strings.Contains(body, "first") {
		t.Errorf("collection: %d %s", code, body)
	}
	if code, _ := fetch("/users/bob/following?page=1"); code != 403 {
		t.Errorf("page: %d", code)
	}

	// Nothing else gives the lists away.
	if len(la.familiarFollowers(ann, cat, 10)) != 0 {
		t.Error("familiar followers show whom bob follows")
	}
	for _, s := range la.suggestions(ann, 10) {
		if s.Account.ID == cat.ID && s.Source == "friends_of_friends" {
			t.Error("suggestions show whom bob follows")
		}
	}

	// The web lists say so; bob still sees his own.
	page := func(user, path string) string {
		resp, err := webClient(t, la, user, "password123").Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if p := page("ann", "/@bob/following"); !strings.Contains(p, "keeps who they follow private") || strings.Contains(p, "@cat") {
		t.Error("ann sees bob's follows")
	}
	if p := page("bob", "/@bob/following"); !strings.Contains(p, "Only you can see this list") || !strings.Contains(p, "/@cat") {
		t.Error("bob can't see his own follows")
	}
	if p := page("bob", "/settings"); !strings.Contains(p, `name="hide_collections" checked`) {
		t.Error("settings don't show the option")
	}
}
