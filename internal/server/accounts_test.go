package server

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/slopinger/trough/internal/ap"
)

func TestIncomingMove(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	// alice moves to alice2 on the same server; alice2 lists alice as an alias
	newActor := strings.NewReplacer("users/alice", "users/alice2", `"preferredUsername": "alice"`, `"preferredUsername": "alice2"`,
		`"alsoKnownAs": []`, `"alsoKnownAs": ["{{R}}/users/alice"]`).Replace(mastodonActor)
	r.put("/users/alice2", newActor)
	key := r.Base + "/users/alice#main-key"
	// a move to an account that doesn't list the old one is ignored
	r.put("/users/mallory", strings.NewReplacer("users/alice", "users/mallory", `"preferredUsername": "alice"`, `"preferredUsername": "mallory"`).Replace(mastodonActor))
	r.send(obj{"@context": asContext, "id": r.Base + "/moves/0", "type": "Move", "actor": alice.URI, "object": alice.URI, "target": r.Base + "/users/mallory"}, key, sigCavage)
	r.send(obj{"@context": asContext, "id": r.Base + "/moves/1", "type": "Move", "actor": alice.URI, "object": alice.URI, "target": r.Base + "/users/alice2"}, key, sigCavage)
	eventually(t, "Follow sent to the new account", func() bool {
		for _, f := range r.receivedOf("Follow") {
			if ap.ID(f.Activity["object"]) == r.Base+"/users/alice2" {
				return true
			}
		}
		return false
	})
	for _, f := range r.receivedOf("Follow") {
		if ap.ID(f.Activity["object"]) == r.Base+"/users/mallory" {
			t.Fatal("followed a move target without an alias")
		}
	}
	if la.relationship(bob.ID, alice.ID).Following {
		t.Error("still following the old account")
	}
	old, _ := la.AccountByID(alice.ID)
	if old.MovedTo != r.Base+"/users/alice2" {
		t.Errorf("moved_to = %q", old.MovedTo)
	}
	if la.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=? AND type='move'`, bob.ID) != 1 {
		t.Error("no move notification")
	}
}

func TestOutgoingMoveNeedsAlias(t *testing.T) {
	la, r, bob, _ := featureFixture(t)
	ctx := context.Background()
	if _, err := la.MoveTo(ctx, bob, r.Base+"/users/alice"); err == nil {
		t.Fatal("moved to an account without alias")
	}
	r.put("/users/alice", strings.Replace(mastodonActor, `"alsoKnownAs": []`, `"alsoKnownAs": ["`+bob.URI+`"]`, 1))
	if _, err := la.MoveTo(ctx, bob, r.Base+"/users/alice"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Move delivered", func() bool { return len(r.receivedOf("Move")) == 1 })
	mv := r.receivedOf("Move")[0]
	if mv.SigErr != nil || ap.ID(mv.Activity["target"]) != r.Base+"/users/alice" || ap.ID(mv.Activity["object"]) != bob.URI {
		t.Fatalf("Move = %v (%v)", mv.Activity, mv.SigErr)
	}
	_, actor := r.signedGet(bob.URI, "")
	if ap.Str(actor, "movedTo") != r.Base+"/users/alice" {
		t.Errorf("actor movedTo = %v", actor["movedTo"])
	}
}

func TestSignupModes(t *testing.T) {
	la := startLiveApp(t)
	admin, _ := la.CreateLocalUser("boss", "password123", true)
	signup := func(form url.Values) (int, string) {
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar}
		req, _ := http.NewRequest("POST", la.base+"/signup", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		req.Header.Set("X-Forwarded-For", form.Get("username")+".example") // distinct rate-limit keys
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	form := func(user, invite string) url.Values {
		return url.Values{"username": {user}, "password": {"password123"}, "confirm": {"password123"}, "rules": {"1"}, "invite": {invite}}
	}
	if code, _ := signup(form("eve", "")); code != 403 {
		t.Errorf("closed: %d", code)
	}
	invite, _ := la.CreateInvite(admin, 1, 0)
	la.setSetting("registrations", "invite")
	if code, _ := signup(form("eve", "wrong")); code != 403 {
		t.Errorf("bad invite: %d", code)
	}
	if code, body := signup(form("eve", invite)); code != 200 || !strings.Contains(body, "Welcome") {
		t.Errorf("invite signup: %d", code)
	}
	if code, _ := signup(form("frank", invite)); code != 403 {
		t.Errorf("used-up invite accepted: %d", code)
	}
	la.setSetting("registrations", "open")
	if code, _ := signup(form("grace", "")); code != 200 {
		t.Errorf("open signup: %d", code)
	}
	if code, _ := signup(form("boss", "")); code != 400 {
		t.Errorf("duplicate username: %d", code)
	}
	if _, err := la.LocalAccount("grace"); err != nil {
		t.Error("account not created")
	}
}

func TestUnansweredFollowIsResentOnce(t *testing.T) {
	la, r, bob, _ := featureFixture(t) // bob already follows alice (accepted)
	r.put("/users/carol", strings.NewReplacer("users/alice", "users/carol", `"preferredUsername": "alice"`, `"preferredUsername": "carol"`).Replace(mastodonActor))
	carol, err := la.getAccount(context.Background(), r.Base+"/users/carol")
	if err != nil {
		t.Fatal(err)
	}
	la.db.Exec(`UPDATE follows SET created_at=? WHERE account_id=?`, time.Now().Add(-200*time.Second).Unix(), bob.ID)
	if err := la.Follow(bob, carol); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Follow delivered", func() bool { return len(r.receivedOf("Follow")) == 1 })
	first := r.receivedOf("Follow")[0].Activity["id"]
	start := time.Now()
	for _, after := range []time.Duration{time.Minute, 2 * time.Minute, 3*time.Minute + 30*time.Second, 4*time.Minute + 30*time.Second} {
		la.resendPendingFollows(start.Add(after))
	}
	eventually(t, "Follow resent", func() bool { return len(r.receivedOf("Follow")) >= 2 })
	time.Sleep(300 * time.Millisecond)
	follows := r.receivedOf("Follow")
	if len(follows) != 2 || ap.ID(follows[1].Activity["object"]) != carol.URI {
		t.Fatalf("expected the Follow to carol once more (and not the accepted one to alice), got %d", len(follows))
	}
	second := follows[1].Activity["id"].(string)
	if second == first {
		t.Fatal("resent Follow reuses the old id; Lemmy drops it as a duplicate")
	}
	// the Accept answers the new id
	r.send(obj{"@context": asContext, "id": r.Base + "/accepts/1", "type": "Accept", "actor": carol.URI, "object": second}, r.Base+"/users/carol#main-key", sigCavage)
	eventually(t, "follow accepted", func() bool { return la.relationship(bob.ID, carol.ID).Following })
}

// ListUsers feeds the admin user list and `trough user list`.
func TestListUsers(t *testing.T) {
	la := startLiveApp(t)
	la.CreateLocalUser("bob", "password123", true)
	la.CreateLocalUser("cat", "password123", false)
	users, err := la.ListUsers()
	if err != nil || len(users) != 2 || users[0].Account.Username != "bob" || !users[0].Admin || users[1].Admin {
		t.Fatalf("ListUsers = %v, %v", users, err)
	}
}
