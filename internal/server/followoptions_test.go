package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestFollowOptions(t *testing.T) {
	la := startLiveApp(t)
	la.CreateLocalUser("bob", "password123", false)
	cat, _ := la.CreateLocalUser("cat", "password123", false)
	dan, _ := la.CreateLocalUser("dan", "password123", false)
	ctx := context.Background()
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write follow")}

	rel := c.ok("POST", "/api/v1/accounts/"+cat.ID+"/follow", url.Values{"notify": {"true"}, "reblogs": {"false"}}).(map[string]any)
	if rel["following"] != true || rel["notifying"] != true || rel["showing_reblogs"] != false {
		t.Fatalf("relationship = %v", rel)
	}
	danPost, _ := la.PostStatus(ctx, dan, PostInput{Text: "dan's thoughts", Visibility: "public"})
	la.PostStatus(ctx, cat, PostInput{Text: "cat's new post", Visibility: "public"})
	la.PostStatus(ctx, cat, PostInput{Text: "@dan a reply to dan", Visibility: "public", InReplyTo: danPost.ID})
	la.PostStatus(ctx, cat, PostInput{Text: "@dan psst", Visibility: "direct"})
	la.Boost(cat, danPost)

	var got []string
	for _, n := range c.ok("GET", "/api/v1/notifications", nil).([]any) {
		m := n.(map[string]any)
		got = append(got, m["type"].(string)+":"+plainText(m["status"].(map[string]any)["content"].(string)))
	}
	if len(got) != 1 || got[0] != "status:cat's new post" {
		t.Errorf("notifications = %q", got)
	}
	var home []string
	for _, s := range c.ok("GET", "/api/v1/timelines/home", nil).([]any) {
		m := s.(map[string]any)
		if m["reblog"] != nil {
			home = append(home, "boost")
		} else {
			home = append(home, plainText(m["content"].(string)))
		}
	}
	if strings.Contains(strings.Join(home, "|"), "boost") || !strings.Contains(strings.Join(home, "|"), "cat's new post") {
		t.Errorf("home = %q", home)
	}

	// The profile's bell and boosts toggle switch the options back.
	web := webClient(t, la, "bob", "password123")
	resp, err := web.Get(la.base + "/@cat")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(b)
	if !strings.Contains(page, "Stop notifying me when cat posts") || !strings.Contains(page, "Show boosts from cat") {
		t.Fatal("profile lacks the bell or the boosts toggle")
	}
	csrf := strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0]
	for _, action := range []string{"unnotify", "show_boosts"} {
		req, _ := http.NewRequest("POST", la.base+"/accounts/"+cat.ID+"/"+action, strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		if resp, err := web.Do(req); err != nil {
			t.Fatal(err)
		} else {
			resp.Body.Close()
		}
	}
	rels := c.ok("GET", "/api/v1/accounts/relationships?id[]="+cat.ID, nil).([]any)
	if r := rels[0].(map[string]any); r["notifying"] != false || r["showing_reblogs"] != true {
		t.Errorf("after web toggles: %v", r)
	}
}
