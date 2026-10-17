package server

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAnnualReport(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	carol, _ := la.CreateLocalUser("carol", "password123", false)
	la.Follow(carol, bob)
	la.Follow(bob, carol)
	p1, _ := la.PostStatus(ctx, bob, PostInput{Text: "fresh #bread", Visibility: "public"})
	p2, _ := la.PostStatus(ctx, bob, PostInput{Text: "more #bread and #butter", Visibility: "public"})
	la.PostStatus(ctx, bob, PostInput{Text: "a thread", Visibility: "public", InReplyTo: p2.ID}) // replying to yourself is a thread
	c1, _ := la.PostStatus(ctx, carol, PostInput{Text: "hello", Visibility: "public"})
	la.PostStatus(ctx, bob, PostInput{Text: "@carol hi", Visibility: "public", InReplyTo: c1.ID})
	la.Boost(bob, c1)
	la.Like(carol, p1)
	la.Boost(carol, p1)
	la.PostStatus(ctx, carol, PostInput{Text: "@bob nice", Visibility: "public", InReplyTo: p2.ID})

	year := time.Now().UTC().Year()
	r := la.annualReport(bob, year)
	if r.Total != 5 || r.Standalone != 3 || r.Replies != 1 || r.Boosts != 1 || r.Archetype != "lurker" || !r.SoFar {
		t.Fatalf("report: %+v", r)
	}
	if len(r.TopTags) != 2 || r.TopTags[0].Name != "bread" || r.TopTags[0].Count != 2 {
		t.Errorf("tags: %+v", r.TopTags)
	}
	if r.TopByBoosts == nil || r.TopByBoosts.Status.ID != p1.ID || r.TopByFavs.Status.ID != p1.ID || r.TopByReplies.Status.ID != p2.ID {
		t.Errorf("top posts: %+v %+v %+v", r.TopByBoosts, r.TopByFavs, r.TopByReplies)
	}
	if tp := r.TopPosts(); len(tp) != 2 || tp[0].Label != "Most boosted (1 boost) · Most favourited (1 favourite)" {
		t.Errorf("merged top posts: %+v", tp)
	}
	if len(r.Interacted) != 1 || r.Interacted[0].Account.ID != carol.ID || r.Interacted[0].Count != 2 {
		t.Errorf("interacted: %+v", r.Interacted)
	}
	if r.NewFollowers != 1 || r.NewFollowing != 1 || r.Months[time.Now().UTC().Month()-1].Posts != 5 {
		t.Errorf("follows %d/%d, months %+v", r.NewFollowers, r.NewFollowing, r.Months)
	}
	if r.PostsPercentile != 50 {
		t.Errorf("posts percentile %d", r.PostsPercentile)
	}

	for want, rep := range map[string]*AnnualReport{
		"booster":  {Total: 30, Boosts: 25, Standalone: 5},
		"pollster": {Total: 20, Standalone: 20, Polls: 3},
		"replier":  {Total: 20, Replies: 15, Standalone: 5},
		"oracle":   {Total: 20, Standalone: 15, Replies: 5},
	} {
		if got := archetype(rep); got != want {
			t.Errorf("archetype %s, want %s", got, want)
		}
	}

	// the API answers like Mastodon
	api := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	y := strconv.Itoa(year)
	all := api.ok("GET", "/api/v1/annual_reports", nil).(map[string]any)
	rep := all["annual_reports"].([]any)[0].(map[string]any)
	data := rep["data"].(map[string]any)
	if rep["year"] != float64(year) || data["archetype"] != "lurker" || data["top_statuses"].(map[string]any)["by_reblogs"] != p1.ID ||
		len(all["statuses"].([]any)) != 2 || len(all["accounts"].([]any)) != 1 {
		t.Errorf("api report: %v", all)
	}
	if s := api.ok("GET", "/api/v1/annual_reports/"+y+"/state", nil).(map[string]any); s["state"] != "available" {
		t.Errorf("state: %v", s)
	}
	if code, _, _ := api.do("GET", "/api/v1/annual_reports/1999", nil); code != 404 {
		t.Errorf("1999: %d", code)
	}
	api.ok("POST", "/api/v1/annual_reports/"+y+"/read", nil)

	// the web page, and the Home banner from December to mid-January
	resp, _ := webClient(t, la, "bob", "password123").Get(la.base + "/annual_report")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if page := string(b); !strings.Contains(page, "Your "+y) || !strings.Contains(page, "The lurker") || !strings.Contains(page, "#bread") {
		t.Error("web report incomplete")
	}
	for when, want := range map[time.Time]int{
		time.Date(year, 12, 5, 0, 0, 0, 0, time.UTC):   year,
		time.Date(year+1, 1, 10, 0, 0, 0, 0, time.UTC): year,
		time.Date(year+1, 1, 20, 0, 0, 0, 0, time.UTC): 0,
		time.Date(year, 6, 1, 0, 0, 0, 0, time.UTC):    0,
	} {
		if got := la.reportBannerYear(bob, when); got != want {
			t.Errorf("banner on %s: %d, want %d", when.Format("2006-01-02"), got, want)
		}
	}
}
