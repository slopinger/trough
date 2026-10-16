package server

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestAdminWeekStats(t *testing.T) {
	la, _, bob, alice := featureFixture(t)
	la.CreateLocalUser("boss", "password123", true)
	ctx := context.Background()
	mine, _ := la.PostStatus(ctx, bob, PostInput{Text: "hello", Visibility: "public"})
	cat, _ := la.CreateLocalUser("cat", "password123", false)
	la.Like(cat, mine)
	la.Boost(cat, mine)
	la.PostStatus(ctx, cat, PostInput{Text: "hi bob", Visibility: "public", InReplyTo: mine.ID})
	la.PostStatus(ctx, bob, PostInput{Text: "talking to myself", Visibility: "public", InReplyTo: mine.ID})
	la.insertStatus(&Status{ID: "remote1", URI: alice.URI + "/s/1", AccountID: alice.ID, Visibility: "public", Content: "<p>hi</p>", CreatedAt: mine.CreatedAt})

	web := webClient(t, la, "boss", "password123")
	resp, err := web.Get(la.base + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(b)
	for _, want := range []string{
		"<b>3</b> new users",
		"<b>3</b> local posts",
		"<b>3</b> likes, boosts and replies on local posts", // bob's self-reply doesn't count
		"<b>1</b> remote accounts discovered",
		"<b>" + alice.Domain + "</b> (1)",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("admin page lacks %q", want)
		}
	}
}
