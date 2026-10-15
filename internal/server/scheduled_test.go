package server

import (
	"context"
	"net/url"
	"testing"
	"time"
)

func TestScheduledPosts(t *testing.T) {
	la, r, bob, _ := featureFixture(t)
	token := oauthLogin(t, la, "bob", "password123", "read write")
	c := &apiClient{t: t, base: la.base, token: token}
	soon := time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339)
	if code, _, _ := c.do("POST", "/api/v1/statuses", url.Values{"status": {"too soon"}, "scheduled_at": {soon}}); code != 422 {
		t.Errorf("schedule within 5 minutes: %d", code)
	}
	later := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	s := c.ok("POST", "/api/v1/statuses", map[string]any{"status": "from the future", "scheduled_at": later, "visibility": "public",
		"poll": map[string]any{"options": []string{"a", "b"}, "expires_in": 600}}).(map[string]any)
	if s["params"].(map[string]any)["text"] != "from the future" || s["scheduled_at"] == nil {
		t.Fatalf("scheduled = %v", s)
	}
	if list := c.ok("GET", "/api/v1/scheduled_statuses", nil).([]any); len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	moved := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	if u := c.ok("PUT", "/api/v1/scheduled_statuses/"+s["id"].(string), url.Values{"scheduled_at": {moved}}).(map[string]any); u["scheduled_at"] == s["scheduled_at"] {
		t.Error("reschedule didn't change the time")
	}
	// scheduled media survive orphan cleanup
	at, _ := la.saveUpload(bob, pngBytes(), "pic")
	la.Schedule(bob, PostInput{Text: "with pic", Visibility: "public", MediaIDs: []string{at.ID}}, time.Now().Add(time.Hour))
	la.db.Exec(`UPDATE attachments SET created_at=0 WHERE id=?`, at.ID)
	la.cleanOrphanMedia()
	if la.countInt(`SELECT COUNT(*) FROM attachments WHERE id=?`, at.ID) != 1 {
		t.Fatal("scheduled media removed by cleanup")
	}
	// make everything due and publish
	la.db.Exec(`UPDATE scheduled_statuses SET scheduled_at=?`, time.Now().Add(-time.Second).Unix())
	la.publishDue(context.Background())
	if n := la.countInt(`SELECT COUNT(*) FROM scheduled_statuses`); n != 0 {
		t.Fatalf("%d still scheduled", n)
	}
	sts, _ := la.accountTimeline(bob, bob, true, false, "")
	if len(sts) != 2 {
		t.Fatalf("published %d posts", len(sts))
	}
	var poll *Status
	for _, st := range sts {
		if st.Poll != nil {
			poll = st
		}
		if st.Content == "" {
			continue
		}
	}
	if poll == nil || time.Until(poll.Poll.EndTime) < 9*time.Minute {
		t.Errorf("poll duration not counted from publishing: %+v", poll)
	}
	withPic := 0
	for _, st := range sts {
		withPic += len(st.Attachments)
	}
	if withPic != 1 {
		t.Errorf("attachments on published posts = %d", withPic)
	}
	eventually(t, "published posts federated", func() bool { return len(r.receivedOf("Create")) == 2 })
	if parseScheduleTime2("2026-05-01T10:00") != "2026-05-01T10:00:00Z" {
		t.Error("datetime-local fallback not taken as UTC")
	}
}

func parseScheduleTime2(s string) string {
	t, _ := parseScheduleTime(s)
	return t.UTC().Format(time.RFC3339)
}
