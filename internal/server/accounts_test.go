package server

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
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
			if idOf(f.Activity["object"]) == r.Base+"/users/alice2" {
				return true
			}
		}
		return false
	})
	for _, f := range r.receivedOf("Follow") {
		if idOf(f.Activity["object"]) == r.Base+"/users/mallory" {
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
	if mv.SigErr != nil || idOf(mv.Activity["target"]) != r.Base+"/users/alice" || idOf(mv.Activity["object"]) != bob.URI {
		t.Fatalf("Move = %v (%v)", mv.Activity, mv.SigErr)
	}
	_, actor := r.signedGet(bob.URI, "")
	if str(actor, "movedTo") != r.Base+"/users/alice" {
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
