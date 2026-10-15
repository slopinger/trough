package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRulesAndAnnouncements(t *testing.T) {
	la := startLiveApp(t)
	la.CreateLocalUser("bob", "password123", false)
	la.addRule("Be kind", "No harassment or hate.")
	la.addRule("No spam", "")
	la.addAnnouncement("Maintenance **tonight**", time.Time{})
	la.addAnnouncement("old news", time.Now().Add(-time.Hour))

	anon := &apiClient{t: t, base: la.base}
	rules := anon.ok("GET", "/api/v1/instance/rules", nil).([]any)
	if len(rules) != 2 || rules[0].(map[string]any)["text"] != "Be kind" || rules[0].(map[string]any)["hint"] != "No harassment or hate." {
		t.Fatalf("rules = %v", rules)
	}
	if inst := anon.ok("GET", "/api/v2/instance", nil).(map[string]any); len(inst["rules"].([]any)) != 2 {
		t.Fatalf("instance rules = %v", inst["rules"])
	}

	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	list := c.ok("GET", "/api/v1/announcements", nil).([]any)
	if len(list) != 1 {
		t.Fatalf("announcements = %v", list)
	}
	a := list[0].(map[string]any)
	if a["content"] != "<p>Maintenance <strong>tonight</strong></p>" || a["read"] != false {
		t.Fatalf("announcement = %v", a)
	}
	id := a["id"].(string)
	c.ok("PUT", "/api/v1/announcements/"+id+"/reactions/👍", nil)
	if code, _, _ := c.do("PUT", "/api/v1/announcements/"+id+"/reactions/nope", nil); code != 422 {
		t.Fatalf("bad reaction: %d", code)
	}
	r := c.ok("GET", "/api/v1/announcements", nil).([]any)[0].(map[string]any)["reactions"].([]any)
	if len(r) != 1 || r[0].(map[string]any)["count"] != float64(1) || r[0].(map[string]any)["me"] != true {
		t.Fatalf("reactions = %v", r)
	}
	c.ok("POST", "/api/v1/announcements/"+id+"/dismiss", nil)
	if a := c.ok("GET", "/api/v1/announcements", nil).([]any)[0].(map[string]any); a["read"] != true {
		t.Fatalf("not read after dismiss: %v", a)
	}
	if got := c.ok("GET", "/api/v1/announcements?with_dismissed=false", nil).([]any); len(got) != 0 {
		t.Fatalf("with_dismissed=false = %v", got)
	}

	resp, err := http.Get(la.base + "/about")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "No harassment or hate.") {
		t.Fatal("rules missing on /about")
	}
}
