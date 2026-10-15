package server

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

func TestSuggestionsAndFamiliarFollowers(t *testing.T) {
	la := startLiveApp(t)
	acc := map[string]*Account{}
	for _, n := range []string{"ann", "ben", "cat", "dan"} {
		acc[n], _ = la.CreateLocalUser(n, "password123", false)
	}
	la.Follow(acc["ann"], acc["ben"])
	la.Follow(acc["ben"], acc["cat"])
	la.Follow(acc["cat"], acc["dan"])
	la.PostStatus(context.Background(), acc["dan"], PostInput{Text: "hi", Visibility: "public"})

	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "ann", "password123", "read write follow")}
	got := c.ok("GET", "/api/v2/suggestions", nil).([]any)
	if len(got) != 2 {
		t.Fatalf("suggestions = %v", got)
	}
	first := got[0].(map[string]any)
	if first["account"].(map[string]any)["id"] != acc["cat"].ID || first["sources"].([]any)[0] != "friends_of_friends" {
		t.Fatalf("first suggestion = %v", first)
	}
	if got[1].(map[string]any)["account"].(map[string]any)["id"] != acc["dan"].ID || got[1].(map[string]any)["sources"].([]any)[0] != "global" {
		t.Fatalf("second suggestion = %v", got[1])
	}
	c.ok("DELETE", "/api/v1/suggestions/"+acc["cat"].ID, nil)
	if got := c.ok("GET", "/api/v1/suggestions", nil).([]any); len(got) != 1 || got[0].(map[string]any)["id"] != acc["dan"].ID {
		t.Fatalf("after dismissing = %v", got)
	}

	ff := c.ok("GET", "/api/v1/accounts/familiar_followers?id[]="+acc["cat"].ID+"&id[]="+acc["dan"].ID, nil).([]any)
	if a := ff[0].(map[string]any)["accounts"].([]any); len(a) != 1 || a[0].(map[string]any)["id"] != acc["ben"].ID {
		t.Fatalf("familiar followers of cat = %v", ff[0])
	}
	if a := ff[1].(map[string]any)["accounts"].([]any); len(a) != 0 {
		t.Fatalf("familiar followers of dan = %v", ff[1])
	}

	resp, err := webClient(t, la, "ann", "password123").Get(la.base + "/@cat")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Followed by") {
		t.Fatal("profile doesn't show familiar followers")
	}
}

// webClient returns an HTTP client logged in to the web UI.
func webClient(t *testing.T, la *liveApp, user, pw string) *http.Client {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	req, _ := http.NewRequest("POST", la.base+"/login", strings.NewReader(url.Values{"username": {user}, "password": {pw}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", la.base)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return c
}
