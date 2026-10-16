package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTrends(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	var users []*Account
	for _, n := range []string{"ann", "ben", "cat"} {
		u, _ := la.CreateLocalUser(n, "password123", false)
		users = append(users, u)
	}
	hot, _ := la.PostStatus(ctx, users[0], PostInput{Text: "big news #Trough", Visibility: "public"})
	meToo, _ := la.PostStatus(ctx, users[1], PostInput{Text: "me too #trough", Visibility: "public"})
	la.PostStatus(ctx, users[2], PostInput{Text: "only me #lonely", Visibility: "public"})
	la.PostStatus(ctx, users[2], PostInput{Text: "secret #trough", Visibility: "private"})
	la.PostStatus(ctx, users[2], PostInput{Text: "boring post", Visibility: "public"})
	la.Like(users[1], hot)
	la.Boost(users[2], hot)

	c := &apiClient{t: t, base: la.base}
	tags := c.ok("GET", "/api/v1/trends/tags", nil).([]any)
	if len(tags) != 1 || tags[0].(map[string]any)["name"] != "trough" {
		t.Fatalf("trending tags = %v", tags)
	}
	today := tags[0].(map[string]any)["history"].([]any)[0].(map[string]any)
	if today["uses"] != "2" || today["accounts"] != "2" {
		t.Fatalf("history = %v", today)
	}
	sts := c.ok("GET", "/api/v1/trends/statuses", nil).([]any)
	if len(sts) != 1 || sts[0].(map[string]any)["id"] != hot.ID {
		t.Fatalf("trending statuses = %v", sts)
	}
	if links := c.ok("GET", "/api/v1/trends/links", nil).([]any); len(links) != 0 {
		t.Fatalf("trending links = %v", links)
	}
	for _, tab := range []string{"", "?tab=tags", "?tab=links"} {
		resp, err := http.Get(la.base + "/explore" + tab)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || (tab == "?tab=tags" && !strings.Contains(string(body), "#trough")) || (tab == "" && !strings.Contains(string(body), "big news")) {
			t.Fatalf("explore%s: %d", tab, resp.StatusCode)
		}
	}
	// The hashtag page sums up the week: the private post doesn't count.
	resp, err := http.Get(la.base + "/tags/trough")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), "<b>2</b> posts from <b>2</b> people this week · <b>2</b> today") {
		t.Errorf("hashtag header missing or wrong")
	}
	// A link shared by two people trends; Explore leads to its posts.
	la.db.Exec(`UPDATE statuses SET card_url='https://news.example/pigs' WHERE id IN (?,?)`, hot.ID, meToo.ID)
	if links := c.ok("GET", "/api/v1/trends/links", nil).([]any); len(links) != 1 {
		t.Fatalf("trending links = %v", links)
	}
	resp, err = http.Get(la.base + "/explore?tab=links")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `href="/links?url=https%3a%2f%2fnews.example%2fpigs"`) {
		t.Errorf("trending link doesn't lead to its posts:\n%s", body)
	}
}
