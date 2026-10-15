package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestGroupedNotifications(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	r.put("/users/carol", strings.NewReplacer("users/alice", "users/carol", `"preferredUsername": "alice"`, `"preferredUsername": "carol"`).Replace(mastodonActor))
	carol, err := la.getAccount(context.Background(), r.Base+"/users/carol")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := la.PostStatus(context.Background(), bob, PostInput{Text: "like me", Visibility: "public"})
	la.notify(bob.ID, "favourite", alice.ID, st.ID)
	la.notify(bob.ID, "favourite", carol.ID, st.ID)
	la.notify(bob.ID, "follow", carol.ID, "")
	la.notify(bob.ID, "mention", alice.ID, st.ID)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}

	res := c.ok("GET", "/api/v2/notifications", nil).(map[string]any)
	groups := res["notification_groups"].([]any)
	if len(groups) != 3 || len(res["accounts"].([]any)) != 2 || len(res["statuses"].([]any)) != 1 {
		t.Fatalf("grouped = %v", res)
	}
	byType := map[string]map[string]any{}
	for _, g := range groups {
		byType[g.(map[string]any)["type"].(string)] = g.(map[string]any)
	}
	fav := byType["favourite"]
	if fav["group_key"] != "favourite-"+st.ID || fav["notifications_count"].(float64) != 2 || len(fav["sample_account_ids"].([]any)) != 2 || fav["status_id"] != st.ID {
		t.Fatalf("favourite group = %v", fav)
	}
	if !strings.HasPrefix(byType["mention"]["group_key"].(string), "ungrouped-") || !strings.HasPrefix(byType["follow"]["group_key"].(string), "follow-") {
		t.Errorf("groups = %v", groups)
	}
	// v1 notifications carry the same group keys
	for _, n := range c.ok("GET", "/api/v1/notifications?types[]=favourite", nil).([]any) {
		if n.(map[string]any)["group_key"] != fav["group_key"] {
			t.Errorf("v1 group_key = %v", n.(map[string]any)["group_key"])
		}
	}
	// grouping can be turned off per type, and types filter
	res = c.ok("GET", "/api/v2/notifications?grouped_types[]=follow", nil).(map[string]any)
	if n := len(res["notification_groups"].([]any)); n != 4 {
		t.Errorf("favourites not ungrouped: %d groups", n)
	}
	res = c.ok("GET", "/api/v2/notifications?types[]=mention", nil).(map[string]any)
	if n := len(res["notification_groups"].([]any)); n != 1 {
		t.Errorf("types filter: %d groups", n)
	}
	// one group, its accounts, then dismiss it
	key := fav["group_key"].(string)
	one := c.ok("GET", "/api/v2/notifications/"+key, nil).(map[string]any)
	if g := one["notification_groups"].([]any); len(g) != 1 || g[0].(map[string]any)["notifications_count"].(float64) != 2 {
		t.Errorf("single group = %v", one)
	}
	if accs := c.ok("GET", "/api/v2/notifications/"+key+"/accounts", nil).([]any); len(accs) != 2 {
		t.Errorf("group accounts = %v", accs)
	}
	c.ok("POST", "/api/v2/notifications/"+key+"/dismiss", nil)
	if code, _, _ := c.do("GET", "/api/v2/notifications/"+key, nil); code != 404 {
		t.Errorf("dismissed group still there: %d", code)
	}
	if n := la.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=?`, bob.ID); n != 2 {
		t.Errorf("%d notifications left, want 2", n)
	}
	if p := c.ok("GET", "/api/v2/notifications/policy", nil).(map[string]any); p["for_not_following"] != "accept" {
		t.Errorf("policy = %v", p)
	}
	if reqs := c.ok("GET", "/api/v1/notifications/requests", nil).([]any); len(reqs) != 0 {
		t.Errorf("requests = %v", reqs)
	}
}

// most_recent_notification_id is a number (as on Mastodon; Ice Cubes needs
// that) and can be sent back as max_id/since_id.
func TestNotificationNumbers(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := newIDAt(time.UnixMilli(int64(i) * 987654321))
		n, ok := ulidNumber(id)
		if !ok || n < 0 {
			t.Fatalf("%s -> %d %v", id, n, ok)
		}
		lo, hi := ulidBound(n, false), ulidBound(n, true)
		if !(lo <= id && id <= hi) {
			t.Fatalf("%s not within [%s, %s]", id, lo, hi)
		}
		if m, _ := ulidNumber(lo); m != n {
			t.Fatalf("bound %s maps to %d, not %d", lo, m, n)
		}
		if b, ok := decodeULID(id); !ok || encodeULID(b) != id {
			t.Fatalf("decode/encode %s", id)
		}
	}
	if _, ok := ulidNumber("not-a-ulid"); ok {
		t.Fatal("parsed garbage")
	}
}
