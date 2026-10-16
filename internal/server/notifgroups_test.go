package server

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestWebNotificationGroups(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	var fans []*Account
	for _, n := range []string{"ann", "cat", "dan", "eve"} {
		a, _ := la.CreateLocalUser(n, "password123", false)
		fans = append(fans, a)
	}
	st, _ := la.PostStatus(ctx, bob, PostInput{Text: "a popular post", Visibility: "public"})
	other, _ := la.PostStatus(ctx, bob, PostInput{Text: "a quiet post", Visibility: "public"})
	for _, a := range fans {
		la.Like(a, st)
	}
	la.Like(fans[0], other)
	la.PostStatus(ctx, fans[1], PostInput{Text: "@bob hi", Visibility: "public"})
	la.PostStatus(ctx, fans[2], PostInput{Text: "@bob hello", Visibility: "public"})

	ns, _ := la.notifications(bob, "")
	groups := groupNotifications(ns)
	// two mentions stay separate; four likes of one post fold into one; the other like stays
	if len(ns) != 7 || len(groups) != 4 {
		t.Fatalf("%d notifications in %d groups, want 7 in 4", len(ns), len(groups))
	}
	for _, g := range groups {
		if g.Type == "favourite" && g.Status.ID == st.ID && (g.From.ID != fans[3].ID || len(g.Others) != 3) {
			t.Errorf("like group: newest %s, others %d", g.From.Username, len(g.Others))
		}
	}

	resp, err := webClient(t, la, "bob", "password123").Get(la.base + "/notifications")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	if !strings.Contains(page, `>eve</a>, <a href="/@dan" class="dn">dan</a> and 2 others`) {
		t.Errorf("grouped like not shown as \"eve, dan and 2 others\"")
	}
	if strings.Count(page, "favourited your post") != 2 || strings.Count(page, "mentioned you") != 2 {
		t.Errorf("expected 2 like rows and 2 mentions, got %d and %d", strings.Count(page, "favourited your post"), strings.Count(page, "mentioned you"))
	}
}
