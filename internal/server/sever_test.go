package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSeveredRelationships(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	carol, _ := la.CreateLocalUser("carol", "password123", false)
	la.CreateLocalUser("dan", "password123", false)
	r := newRemote(t, la)
	r.put("/users/alice", mastodonActor)
	r.put("/users/eve", strings.ReplaceAll(strings.ReplaceAll(mastodonActor, "users/alice", "users/eve"), `"alice"`, `"eve"`))
	alice, _ := la.getAccount(ctx, r.Base+"/users/alice")
	eve, _ := la.getAccount(ctx, r.Base+"/users/eve")
	follow := func(id, from, to string) {
		la.db.Exec(`INSERT INTO follows (id,uri,account_id,target_id,accepted,created_at,show_reblogs) VALUES (?,?,?,?,1,0,0)`, id, "x"+id, from, to)
	}
	follow("f1", bob.ID, alice.ID)
	follow("f2", alice.ID, bob.ID)
	follow("f3", eve.ID, bob.ID)
	follow("f4", carol.ID, eve.ID)

	// suspending one remote account
	la.SuspendAccount(alice)
	ns, _ := la.notifications(bob, "")
	if len(ns) != 1 || ns[0].Event == nil || ns[0].Event.Type != "account_suspension" || ns[0].Event.Followers != 1 || ns[0].Event.Following != 1 {
		t.Fatalf("bob after suspension: %+v", ns)
	}

	// blocking the server takes the rest
	if err := la.BlockServer(r.Host, "spam"); err != nil {
		t.Fatal(err)
	}
	if n := la.countInt(`SELECT COUNT(*) FROM follows`); n != 0 {
		t.Fatalf("%d follows left", n)
	}
	ns, _ = la.notifications(carol, "")
	if len(ns) != 1 || ns[0].Event.Type != "domain_block" || ns[0].Event.TargetName != r.Host || ns[0].Event.Following != 1 || ns[0].Event.Followers != 0 {
		t.Fatalf("carol after the block: %+v", ns)
	}

	// clients get Mastodon's event entity
	api := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read")}
	list := api.ok("GET", "/api/v1/notifications", nil).([]any)
	ev := list[0].(map[string]any)["event"].(map[string]any)
	if list[0].(map[string]any)["type"] != "severed_relationships" || ev["type"] != "domain_block" || ev["followers_count"] != float64(1) {
		t.Fatalf("api: %v", list[0])
	}

	// the page offers the lost follows in the import format
	get := func(user, path string) (int, string) {
		resp, err := webClient(t, la, user, "password123").Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if _, page := get("bob", "/severed_relationships"); !strings.Contains(page, "suspended @alice@"+r.Host) || !strings.Contains(page, "blocked "+r.Host) {
		t.Error("page lacks the events")
	}
	sev := la.severances(bob)
	var suspension string
	for _, s := range sev {
		if s.Type == "account_suspension" {
			suspension = s.ID
		}
	}
	code, csv := get("bob", "/severed_relationships/"+suspension+"/following.csv")
	if code != 200 || csv != "Account address,Show boosts,Notify on new posts,Languages\nalice@"+r.Host+",false,false,\n" {
		t.Errorf("following.csv: %d %q", code, csv)
	}
	if code, _ := get("dan", "/severed_relationships/"+suspension+"/following.csv"); code != http.StatusNotFound {
		t.Errorf("dan downloaded bob's list: %d", code)
	}
}
