package server

import (
	"context"
	"testing"
)

func TestNotificationPolicy(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	friend, _ := la.CreateLocalUser("friend", "password123", false)
	stranger, _ := la.CreateLocalUser("stranger", "password123", false)
	la.Follow(bob, friend)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	count := func() int { return len(c.ok("GET", "/api/v1/notifications", nil).([]any)) }

	// default: unsolicited private mentions are filtered, everything else shown
	la.PostStatus(ctx, friend, PostInput{Text: "@bob psst", Visibility: "direct"})
	la.PostStatus(ctx, stranger, PostInput{Text: "@bob hello stranger", Visibility: "direct"})
	la.PostStatus(ctx, stranger, PostInput{Text: "@bob public hi", Visibility: "public"})
	if n := count(); n != 2 {
		t.Fatalf("visible notifications = %d, want the friend's DM and the public mention", n)
	}
	pol := c.ok("GET", "/api/v2/notifications/policy", nil).(map[string]any)
	if pol["for_private_mentions"] != "filter" || pol["summary"].(map[string]any)["pending_requests_count"] != float64(1) {
		t.Fatalf("policy = %v", pol)
	}
	reqs := c.ok("GET", "/api/v1/notifications/requests", nil).([]any)
	if len(reqs) != 1 || reqs[0].(map[string]any)["account"].(map[string]any)["id"] != stranger.ID || reqs[0].(map[string]any)["notifications_count"] != "1" {
		t.Fatalf("requests = %v", reqs)
	}
	if got := c.ok("GET", "/api/v1/notifications?include_filtered=true", nil).([]any); len(got) != 3 {
		t.Fatalf("include_filtered = %d", len(got))
	}

	// accepting shows them and lets the stranger through from now on
	c.ok("POST", "/api/v1/notifications/requests/"+stranger.ID+"/accept", nil)
	la.PostStatus(ctx, stranger, PostInput{Text: "@bob again", Visibility: "direct"})
	if n := count(); n != 4 {
		t.Fatalf("after accepting: %d", n)
	}

	// drop: notifications from people bob doesn't follow vanish
	c.ok("PATCH", "/api/v2/notifications/policy", map[string]any{"for_not_following": "drop"})
	other, _ := la.CreateLocalUser("other", "password123", false)
	la.PostStatus(ctx, other, PostInput{Text: "@bob hey", Visibility: "public"})
	if n := la.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=? AND from_account_id=?`, bob.ID, other.ID); n != 0 {
		t.Fatalf("dropped notification stored: %d", n)
	}
	if code, _, _ := c.do("PATCH", "/api/v2/notifications/policy", map[string]any{"for_new_accounts": "maybe"}); code != 422 {
		t.Fatalf("bad value: %d", code)
	}
}
