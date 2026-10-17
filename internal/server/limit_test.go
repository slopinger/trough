package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestLimitAccount(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	boss, _ := la.CreateLocalUser("boss", "password123", true)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	carol, _ := la.CreateLocalUser("carol", "password123", false)
	spam, _ := la.CreateLocalUser("spam", "password123", false)
	la.db.Exec(`UPDATE accounts SET indexable=1 WHERE id=?`, spam.ID)
	la.Follow(carol, spam)
	post, _ := la.PostStatus(ctx, spam, PostInput{Text: "cheap #pills here", Visibility: "public"})

	// bob reports it, the admin limits the account
	reportID, err := la.FileReport(bob, spam, []string{post.ID}, "spam", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := la.ResolveReport(boss, reportID, "limit", "spammy"); err != nil {
		t.Fatal(err)
	}
	spam, _ = la.AccountByID(spam.ID)
	if !spam.Limited || spam.Suspended {
		t.Fatalf("limited %v suspended %v", spam.Limited, spam.Suspended)
	}

	has := func(sts []*Status) bool {
		for _, s := range sts {
			if s.ID == post.ID {
				return true
			}
		}
		return false
	}
	if sts, _ := la.localTimeline(bob, ""); has(sts) {
		t.Error("bob still sees the limited post in the local timeline")
	}
	if sts, _ := la.tagTimeline(nil, "pills", ""); has(sts) {
		t.Error("visitors still see it on the hashtag page")
	}
	if has(la.searchStatuses(bob, "cheap", "", "", 20)) {
		t.Error("search still finds it")
	}
	if sts, _ := la.localTimeline(carol, ""); !has(sts) {
		t.Error("carol, who follows spam, lost the post")
	}
	if sts, _ := la.localTimeline(spam, ""); !has(sts) {
		t.Error("spam doesn't see its own post")
	}

	// mentions of people who don't follow it are filtered
	la.PostStatus(ctx, spam, PostInput{Text: "@bob buy now", Visibility: "public"})
	la.PostStatus(ctx, spam, PostInput{Text: "@carol buy now", Visibility: "public"})
	if n := la.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=? AND from_account_id=? AND filtered=1`, bob.ID, spam.ID); n != 1 {
		t.Errorf("bob's notification from the limited account: %d filtered", n)
	}
	if n := la.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=? AND from_account_id=? AND type='mention' AND filtered=0`, carol.ID, spam.ID); n != 1 {
		t.Errorf("carol's mention was filtered: %d unfiltered", n)
	}

	// the profile shows a notice instead of the posts, with a way through
	get := func(path string) string {
		resp, err := http.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if p := get("/@spam"); !strings.Contains(p, "have limited this account") || strings.Contains(p, "cheap") {
		t.Error("limited profile shows the posts without a notice")
	}
	if p := get("/@spam?show=1"); !strings.Contains(p, "cheap") {
		t.Error("show=1 doesn't show the posts")
	}

	// admins see the list and can lift it
	web := webClient(t, la, "boss", "password123")
	resp, _ := web.Get(la.base + "/admin")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), `action="/admin/accounts/`+spam.ID+`/unlimit"`) {
		t.Fatal("admin page doesn't list the limited account")
	}
	if err := la.setLimited(spam, false); err != nil {
		t.Fatal(err)
	}
	if sts, _ := la.localTimeline(bob, ""); !has(sts) {
		t.Error("the post didn't come back after lifting the limit")
	}

	// refetching a remote actor keeps the limit
	r := newRemote(t, la)
	r.put("/users/alice", mastodonActor)
	alice, _ := la.getAccount(ctx, r.Base+"/users/alice")
	la.setLimited(alice, true)
	if alice, err = la.fetchAccount(ctx, alice.URI); err != nil || !alice.Limited {
		t.Errorf("refetch lifted the limit: %v", err)
	}
}
