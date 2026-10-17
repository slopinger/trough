package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestIndexableAndNoindex(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	ann, _ := la.CreateLocalUser("ann", "password123", false)
	st, _ := la.PostStatus(ctx, bob, PostInput{Text: "keep me off google", Visibility: "public"})
	annSt, _ := la.PostStatus(ctx, ann, PostInput{Text: "index me", Visibility: "public"})

	// both are opt-in: the actor says indexable false until bob chooses otherwise
	r := newRemote(t, la)
	if _, actor := r.signedGet(bob.URI, ""); actor["indexable"] != false {
		t.Fatalf("new actor indexable = %v", actor["indexable"])
	}

	// the profile settings form switches both on
	web := webClient(t, la, "bob", "password123")
	get := func(c *http.Client, path string) (*http.Response, string) {
		t.Helper()
		resp, err := c.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	_, settings := get(web, "/settings")
	if !strings.Contains(settings, `name="indexable"`) || !strings.Contains(settings, `name="noindex"`) {
		t.Fatal("settings lack the search options")
	}
	csrf := strings.SplitN(strings.SplitN(settings, `name="csrf" value="`, 2)[1], `"`, 2)[0]
	req, _ := http.NewRequest("POST", la.base+"/settings/profile", strings.NewReader(url.Values{"csrf": {csrf}, "display_name": {"Bob"},
		"indexable": {"on"}, "noindex": {"on"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", la.base)
	if resp, err := web.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	if _, actor := r.signedGet(bob.URI, ""); actor["indexable"] != true {
		t.Errorf("actor indexable after opting in = %v", actor["indexable"])
	}
	api := &apiClient{t: t, base: la.base}
	if a := api.ok("GET", "/api/v1/accounts/"+bob.ID, nil).(map[string]any); a["indexable"] != true || a["noindex"] != true {
		t.Errorf("api account: indexable %v noindex %v", a["indexable"], a["noindex"])
	}

	// search engines are asked to skip bob's pages, but not ann's
	for _, path := range []string{"/@bob", st.Path()} {
		resp, body := get(http.DefaultClient, path)
		if !strings.Contains(body, `<meta name="robots" content="noindex, noarchive">`) || resp.Header.Get("X-Robots-Tag") == "" {
			t.Errorf("%s isn't marked noindex", path)
		}
	}
	for _, path := range []string{"/@ann", annSt.Path()} {
		if resp, body := get(http.DefaultClient, path); strings.Contains(body, `name="robots"`) || resp.Header.Get("X-Robots-Tag") != "" {
			t.Errorf("%s is marked noindex", path)
		}
	}

	// remote actors' choice is kept
	r.put("/users/alice", mastodonActor)
	alice, err := la.getAccount(ctx, r.Base+"/users/alice")
	if err != nil || !alice.Indexable {
		t.Errorf("remote indexable not stored: %v %v", err, alice)
	}
}
