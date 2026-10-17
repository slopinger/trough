package server

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWeekStart(t *testing.T) {
	for in, want := range map[string]string{
		"2026-10-18T23:59:00Z": "2026-10-12", // Sunday
		"2026-10-12T00:00:00Z": "2026-10-12", // Monday midnight
		"2026-10-14T12:00:00Z": "2026-10-12",
		"2026-01-01T08:00:00Z": "2025-12-29", // across a year
	} {
		tm, _ := time.Parse(time.RFC3339, in)
		if got := weekStart(tm).Format(dateOnly); got != want {
			t.Errorf("weekStart(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestInstanceActivity(t *testing.T) {
	la := startLiveApp(t)
	boss, _ := la.CreateLocalUser("boss", "password123", true)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	ctx := context.Background()
	now := time.Now()
	thisWeek, lastWeek := weekStart(now), weekStart(now).AddDate(0, 0, -7)

	// two posts and a boost this week, one post last week, one long ago
	p1, _ := la.PostStatus(ctx, bob, PostInput{Text: "one", Visibility: "public"})
	la.PostStatus(ctx, bob, PostInput{Text: "two", Visibility: "public"})
	la.Boost(boss, p1)
	old, _ := la.PostStatus(ctx, bob, PostInput{Text: "last week", Visibility: "public"})
	la.db.Exec(`UPDATE statuses SET created_at=? WHERE id=?`, lastWeek.Add(time.Hour).Unix(), old.ID)
	ancient, _ := la.PostStatus(ctx, bob, PostInput{Text: "ages ago", Visibility: "public"})
	la.db.Exec(`UPDATE statuses SET created_at=? WHERE id=?`, thisWeek.AddDate(0, 0, -7*20).Unix(), ancient.ID)
	la.db.Exec(`UPDATE users SET created_at=? WHERE account_id=?`, lastWeek.Add(time.Hour).Unix(), bob.ID)
	la.markActive(bob.ID, lastWeek.Add(time.Hour))

	// using the web UI counts as activity this week
	web := webClient(t, la, "boss", "password123")
	resp, err := web.Get(la.base + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), `class="activity-chart"`) || !strings.Contains(string(page), "2 this week") || !strings.Contains(string(page), "Week of "+thisWeek.Format("2 Jan")+": 2 posts") {
		t.Error("admin page lacks the activity charts")
	}

	api := &apiClient{t: t, base: la.base}
	weeks := api.ok("GET", "/api/v1/instance/activity", nil).([]any)
	if len(weeks) != 12 {
		t.Fatalf("%d weeks", len(weeks))
	}
	want := []map[string]any{
		{"week": strconv.FormatInt(thisWeek.Unix(), 10), "statuses": "2", "logins": "1", "registrations": "1"},
		{"week": strconv.FormatInt(lastWeek.Unix(), 10), "statuses": "1", "logins": "1", "registrations": "1"},
	}
	for i, w := range want {
		got := weeks[i].(map[string]any)
		for k, v := range w {
			if got[k] != v {
				t.Errorf("week %d %s = %v, want %v", i, k, got[k], v)
			}
		}
	}
	if last := weeks[11].(map[string]any); last["statuses"] != "0" {
		t.Errorf("oldest week: %v", last)
	}
}
