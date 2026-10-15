package server

import (
	"context"
	"testing"
)

func TestMuteConversation(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	ann, _ := la.CreateLocalUser("ann", "password123", false)
	la.Follow(bob, ann)
	root, _ := la.PostStatus(ctx, bob, PostInput{Text: "a thread", Visibility: "public"})
	reply, _ := la.PostStatus(ctx, ann, PostInput{Text: "@bob first reply", Visibility: "public", InReplyTo: root.ID})
	notifs := func() int { return la.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=?`, bob.ID) }
	before := notifs()
	if before == 0 {
		t.Fatal("setup: no notification for the reply")
	}

	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	if st := c.ok("POST", "/api/v1/statuses/"+reply.ID+"/mute", nil).(map[string]any); st["muted"] != true {
		t.Fatalf("muted = %v", st["muted"])
	}
	deeper, _ := la.PostStatus(ctx, ann, PostInput{Text: "@bob more", Visibility: "public", InReplyTo: reply.ID})
	la.Like(ann, root)
	if notifs() != before {
		t.Fatal("notified from a muted conversation")
	}
	if st := c.ok("GET", "/api/v1/statuses/"+deeper.ID, nil).(map[string]any); st["muted"] != true {
		t.Fatal("deeper reply not shown as muted")
	}
	la.PostStatus(ctx, ann, PostInput{Text: "@bob elsewhere", Visibility: "public"})
	if notifs() != before+1 {
		t.Fatal("muting a thread silenced other mentions")
	}
	c.ok("POST", "/api/v1/statuses/"+root.ID+"/unmute", nil)
	la.PostStatus(ctx, ann, PostInput{Text: "@bob back", Visibility: "public", InReplyTo: deeper.ID})
	if notifs() != before+2 {
		t.Fatal("unmuting didn't bring notifications back")
	}
}
