package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTimedMutes(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	cat, _ := la.CreateLocalUser("cat", "password123", false)
	dan, _ := la.CreateLocalUser("dan", "password123", false)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write follow")}

	// Muting cat for an hour, posts only: her likes still notify.
	rel := c.ok("POST", "/api/v1/accounts/"+cat.ID+"/mute", url.Values{"duration": {"3600"}, "notifications": {"false"}}).(map[string]any)
	if rel["muting"] != true || rel["muting_notifications"] != false {
		t.Fatalf("relationship = %v", rel)
	}
	mutes := c.ok("GET", "/api/v1/mutes", nil).([]any)
	exp, _ := mutes[0].(map[string]any)["mute_expires_at"].(string)
	if at, err := time.Parse(time.RFC3339, exp); err != nil || at.Sub(time.Now()) < 59*time.Minute || at.Sub(time.Now()) > 61*time.Minute {
		t.Fatalf("mute_expires_at = %q", exp)
	}
	post, _ := la.PostStatus(context.Background(), bob, PostInput{Text: "hello", Visibility: "public"})
	la.Like(cat, post)
	la.Like(dan, post)
	notifs := func() int { return len(c.ok("GET", "/api/v1/notifications", nil).([]any)) }
	if n := notifs(); n != 2 {
		t.Errorf("posts-only mute hid notifications: %d", n)
	}
	// Muting again replaces the options: now indefinitely, notifications too.
	rel = c.ok("POST", "/api/v1/accounts/"+cat.ID+"/mute", nil).(map[string]any)
	if rel["muting_notifications"] != true {
		t.Errorf("relationship after re-mute = %v", rel)
	}
	if n := notifs(); n != 1 {
		t.Errorf("full mute shows %d notifications", n)
	}
	if exp := c.ok("GET", "/api/v1/mutes", nil).([]any)[0].(map[string]any)["mute_expires_at"]; exp != nil {
		t.Errorf("indefinite mute expires at %v", exp)
	}

	// The web menu mutes for a day; the settings say until when.
	web := webClient(t, la, "bob", "password123")
	get := func(path string) string {
		resp, err := web.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	page := get("/@dan")
	if !strings.Contains(page, "Mute for 1 day") {
		t.Fatal("profile has no mute menu")
	}
	csrf := strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0]
	req, _ := http.NewRequest("POST", la.base+"/accounts/"+dan.ID+"/mute", strings.NewReader(url.Values{"csrf": {csrf}, "duration": {"86400"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", la.base)
	if resp, err := web.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	if r := la.relationship(bob.ID, dan.ID); !r.Muting || r.MuteExpiresAt.Sub(time.Now()) < 23*time.Hour {
		t.Fatalf("web mute = %+v", r)
	}
	if s := get("/settings"); !strings.Contains(s, "muted until") {
		t.Error("settings don't say how long the mute lasts")
	}
	if p := get("/@dan"); !strings.Contains(p, "Muted until") {
		t.Error("profile doesn't say until when")
	}

	// Timed mutes end on their own.
	la.db.Exec(`UPDATE mutes SET expires_at=? WHERE target_id=?`, time.Now().Add(-time.Second).Unix(), dan.ID)
	la.expireMutes(time.Now())
	if la.relationship(bob.ID, dan.ID).Muting || !la.relationship(bob.ID, cat.ID).Muting {
		t.Error("expiry lifted the wrong mutes")
	}
}
