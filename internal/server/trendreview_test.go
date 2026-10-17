package server

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestTrendReview(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	boss, _ := la.CreateLocalUser("boss", "password123", true)
	var spamPost *Status
	for _, name := range []string{"ann", "bob", "cat"} {
		a, _ := la.CreateLocalUser(name, "password123", false)
		la.PostStatus(ctx, a, PostInput{Text: "fresh #bread", Visibility: "public"})
		spamPost, _ = la.PostStatus(ctx, a, PostInput{Text: "buy #pills https://pills.example/", Visibility: "public"})
		la.db.Exec(`UPDATE statuses SET card_url='https://pills.example/' WHERE id=?`, spamPost.ID)
	}
	ann, _ := la.LocalAccount("ann")
	la.Like(ann, spamPost)
	la.Like(boss, spamPost)

	tags := func() string {
		var names []string
		for _, tg := range la.trendingTags(10, 0) {
			names = append(names, tg.Name)
		}
		return strings.Join(names, ",")
	}
	if got := tags(); got != "pills,bread" && got != "bread,pills" {
		t.Fatalf("trending before review: %q", got)
	}
	if len(la.trendingLinks(10, 0)) != 1 || len(la.trendingStatuses(nil, 10, 0)) != 1 {
		t.Fatal("link or post isn't trending")
	}

	// hiding takes them off Explore and the API
	la.reviewTrend(boss, "tag", "pills", "reject")
	la.reviewTrend(boss, "link", "https://pills.example/", "reject")
	la.reviewTrend(boss, "status", spamPost.ID, "reject")
	if got := tags(); got != "bread" {
		t.Errorf("after hiding: %q", got)
	}
	if len(la.trendingLinks(10, 0)) != 0 || len(la.trendingStatuses(nil, 10, 0)) != 0 {
		t.Error("hidden link or post still trends")
	}
	api := &apiClient{t: t, base: la.base}
	if list := api.ok("GET", "/api/v1/trends/tags", nil).([]any); len(list) != 1 {
		t.Errorf("api trends: %v", list)
	}

	// with review on, only approved ones trend
	la.setSetting("trends_review", "1")
	if got := tags(); got != "" {
		t.Errorf("unreviewed tags trend: %q", got)
	}
	if n := la.pendingTrends(); n != 1 {
		t.Errorf("%d pending, want 1 (bread)", n)
	}
	la.reviewTrend(boss, "tag", "bread", "approve")
	if got := tags(); got != "bread" {
		t.Errorf("after approving: %q", got)
	}

	// the admin page lists everything with its state
	resp, _ := webClient(t, la, "boss", "password123").Get(la.base + "/admin/trends")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(b)
	if !strings.Contains(page, "#pills") || !strings.Contains(page, "hidden") || !strings.Contains(page, "approved") || !strings.Contains(page, "pills.example") {
		t.Error("admin trends page incomplete")
	}
	la.reviewTrend(boss, "tag", "pills", "reset")
	if la.countInt(`SELECT COUNT(*) FROM trend_reviews WHERE kind='tag'`) != 1 {
		t.Error("reset didn't forget the decision")
	}
}
