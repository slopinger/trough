package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestHomeViewSettings(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	carol, _ := la.CreateLocalUser("carol", "password123", false)
	dan, _ := la.CreateLocalUser("dan", "password123", false)
	la.Follow(bob, carol)
	plain, _ := la.PostStatus(ctx, carol, PostInput{Text: "plain post", Visibility: "public"})
	dpost, _ := la.PostStatus(ctx, dan, PostInput{Text: "dan's post", Visibility: "public"})
	la.Boost(carol, dpost)
	toDan, _ := la.PostStatus(ctx, carol, PostInput{Text: "@dan reply to dan", Visibility: "public", InReplyTo: dpost.ID})
	bpost, _ := la.PostStatus(ctx, bob, PostInput{Text: "bob's post", Visibility: "public"})
	toBob, _ := la.PostStatus(ctx, carol, PostInput{Text: "@bob reply to bob", Visibility: "public", InReplyTo: bpost.ID})

	web := webClient(t, la, "bob", "password123")
	home := func() string {
		t.Helper()
		resp, err := web.Get(la.base + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	toggle := func(what string) {
		t.Helper()
		page := home()
		csrf := strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0]
		req, _ := http.NewRequest("POST", la.base+"/home/settings", strings.NewReader(url.Values{"csrf": {csrf}, "toggle": {what}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		resp, err := web.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	ids := func() map[string]bool {
		sts, _ := la.webHomeTimeline(bob, "")
		out := map[string]bool{}
		for _, s := range sts {
			if s.BoostOf != nil {
				out["boost"] = true
			}
			out[s.ID] = true
		}
		return out
	}
	if got := ids(); !got["boost"] || !got[toDan.ID] || !got[toBob.ID] {
		t.Fatalf("default home misses something: %v", got)
	}

	toggle("boosts")
	toggle("replies")
	got := ids()
	if got["boost"] || got[toDan.ID] || !got[toBob.ID] || !got[plain.ID] {
		t.Errorf("with boosts and replies hidden: %v", got)
	}
	if page := home(); !strings.Contains(page, "boosts and replies hidden") || strings.Contains(page, "reply to dan") {
		t.Error("home page doesn't say what's hidden or still shows the reply")
	}
	// apps keep their own settings
	if sts, _ := la.homeTimeline(bob, ""); len(sts) != 5 {
		t.Errorf("api home has %d posts, want 5", len(sts))
	}

	toggle("boosts")
	if got := ids(); !got["boost"] || got[toDan.ID] {
		t.Errorf("after showing boosts again: %v", got)
	}
}
