package server

import (
	"testing"
	"time"

	"github.com/slopinger/trough/internal/ulid"
)

func TestUserDomainBlocks(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write follow")}
	st := &Status{ID: ulid.New(), URI: alice.URI + "/s/1", AccountID: alice.ID, Visibility: "public", Content: "<p>hi</p>", CreatedAt: time.Now()}
	la.insertStatus(st)
	la.db.Exec(`INSERT INTO notifications (id, account_id, type, from_account_id, status_id, created_at) VALUES (?,?,?,?,?,?)`,
		ulid.New(), bob.ID, "favourite", alice.ID, "", time.Now().Unix())
	home := func() int {
		sts, _ := la.homeTimeline(bob, "")
		n := 0
		for _, s := range sts {
			if s.AccountID == alice.ID {
				n++
			}
		}
		return n
	}
	if home() != 1 || len(c.ok("GET", "/api/v1/notifications", nil).([]any)) != 1 {
		t.Fatal("setup: post or notification missing")
	}
	c.ok("POST", "/api/v1/domain_blocks", map[string]any{"domain": "HTTPS://" + alice.Domain + "/"})
	if got := c.ok("GET", "/api/v1/domain_blocks", nil).([]any); len(got) != 1 || got[0] != alice.Domain {
		t.Fatalf("domain blocks = %v", got)
	}
	if home() != 0 || len(c.ok("GET", "/api/v1/notifications", nil).([]any)) != 0 {
		t.Fatal("blocked server still shows up")
	}
	if rel := c.ok("GET", "/api/v1/accounts/relationships?id[]="+alice.ID, nil).([]any)[0].(map[string]any); rel["domain_blocking"] != true || rel["followed_by"] != false {
		t.Fatalf("relationship = %v", rel)
	}
	eventually(t, "Reject of alice's follow", func() bool { return len(r.receivedOf("Reject")) == 1 })
	if code, _, _ := c.do("POST", "/api/v1/domain_blocks", map[string]any{"domain": "not a domain"}); code != 422 {
		t.Fatalf("invalid domain: %d", code)
	}
	c.ok("DELETE", "/api/v1/domain_blocks", map[string]any{"domain": alice.Domain})
	if home() != 1 {
		t.Fatal("unblocking didn't bring the post back")
	}
}
