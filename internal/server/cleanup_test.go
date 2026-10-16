package server

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStatusCleanup(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	cat, _ := la.CreateLocalUser("cat", "password123", false)
	dan, _ := la.CreateLocalUser("dan", "password123", false)
	ctx := context.Background()
	post := func(in PostInput) *Status {
		t.Helper()
		if in.Visibility == "" {
			in.Visibility = "public"
		}
		st, err := la.PostStatus(ctx, bob, in)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	poll, _ := newPoll([]string{"oink", "snort"}, "1d", false)
	plain := post(PostInput{Text: "old and forgettable"})
	pinned := post(PostInput{Text: "old but pinned"})
	dm := post(PostInput{Text: "@cat old secret", Visibility: "direct"})
	withPoll := post(PostInput{Text: "old poll", Poll: poll})
	faved := post(PostInput{Text: "old, and I liked it myself"})
	popular := post(PostInput{Text: "old and loved"})
	catPost, _ := la.PostStatus(ctx, cat, PostInput{Text: "cat's post", Visibility: "public"})
	la.Boost(bob, catPost)
	la.Like(bob, faved)
	la.Like(cat, popular)
	la.Like(dan, popular)
	la.db.Exec(`UPDATE statuses SET pinned_at=1 WHERE id=?`, pinned.ID)
	la.db.Exec(`UPDATE statuses SET created_at=? WHERE account_id=?`, time.Now().AddDate(0, -2, 0).Unix(), bob.ID)
	fresh := post(PostInput{Text: "new"})

	exists := func(st *Status) bool { _, err := la.StatusByID(st.ID); return err == nil }
	la.cleanupStatuses(time.Now())
	if !exists(plain) {
		t.Fatal("deleted without a policy")
	}

	pol := defaultCleanupPolicy()
	pol.Enabled, pol.MinAge, pol.MinFavs = true, 30*86400, 2
	la.setCleanupPolicy(bob.ID, pol)
	if n := la.cleanupCount(bob.ID, la.cleanupPolicy(bob.ID)); n != 3 {
		t.Errorf("%d posts due, want 3 (plain, poll, boost)", n)
	}
	// the settings page says what will happen
	resp, err := webClient(t, la, "bob", "password123").Get(la.base + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "3 of your posts are due to be deleted") || !strings.Contains(string(b), `name="enabled" checked`) {
		t.Error("settings don't show the policy")
	}

	la.cleanupStatuses(time.Now())
	for _, st := range []*Status{plain, withPoll} {
		if exists(st) {
			t.Errorf("%q survived", st.Content)
		}
	}
	for _, st := range []*Status{pinned, dm, faved, popular, fresh, catPost} {
		if !exists(st) {
			t.Errorf("%q was deleted", st.Content)
		}
	}
	if la.countInt(`SELECT COUNT(*) FROM statuses WHERE account_id=? AND boost_of_id != ''`, bob.ID) != 0 {
		t.Error("old boost not undone")
	}
	if n := la.cleanupCount(bob.ID, la.cleanupPolicy(bob.ID)); n != 0 {
		t.Errorf("%d still due", n)
	}
}
