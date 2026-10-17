package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/slopinger/trough/internal/ap"
)

// An admin suspends a local account: it can't log in, its profile and posts
// are hidden, and other servers get an Update with "suspended": true. Lifting
// the suspension brings everything back.
func TestLocalSuspension(t *testing.T) {
	la, r, bob, _ := featureFixture(t)
	ctx := context.Background()
	mod, _ := la.CreateLocalUser("mod", "password123", true)
	st, _ := la.PostStatus(ctx, bob, PostInput{Text: "still here?", Visibility: "public"})
	bobWeb := webClient(t, la, "bob", "password123")
	status := func(c *http.Client, path string) (int, string, string) {
		t.Helper()
		resp, err := c.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Request.URL.Path, string(b)
	}
	if _, path, _ := status(bobWeb, "/home"); path != "/home" {
		t.Fatalf("bob isn't logged in (%s)", path)
	}

	if err := la.AdminSuspend(mod, mod); err == nil {
		t.Error("an admin suspended themselves")
	}
	if err := la.AdminSuspend(mod, bob); err != nil {
		t.Fatal(err)
	}
	if err := la.AdminSuspend(mod, bob); err == nil {
		t.Error("suspended twice")
	}
	eventually(t, "suspension Update delivered", func() bool { return len(r.receivedOf("Update")) == 1 })
	actor := ap.Object(r.receivedOf("Update")[0].Activity["object"])
	if ap.Str(actor, "id") != bob.URI || actor["suspended"] != true || ap.Str(actor, "name") != "" {
		t.Fatalf("Update object = %v", actor)
	}
	if code, doc := r.signedGet(bob.URI, ""); code != 200 || doc["suspended"] != true {
		t.Errorf("actor = %d %v", code, doc)
	}
	for _, u := range []string{bob.URI + "/outbox", st.URI, bob.URI + "/followers"} {
		if code, _ := r.signedGet(u, ""); code != 404 {
			t.Errorf("%s = %d while suspended", u, code)
		}
	}
	if _, path, _ := status(bobWeb, "/home"); path != "/login" {
		t.Errorf("bob's session survived the suspension (%s)", path)
	}
	anon := &http.Client{}
	if code, _, _ := status(anon, "/@bob"); code != 404 {
		t.Errorf("profile = %d", code)
	}
	if _, _, page := status(anon, "/local"); strings.Contains(page, "still here?") {
		t.Error("suspended account's post on the local timeline")
	}
	resp, err := http.PostForm(la.base+"/login", url.Values{"username": {"bob"}, "password": {"password123"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 || !strings.Contains(string(b), "suspended") {
		t.Errorf("login = %d", resp.StatusCode)
	}
	modWeb := webClient(t, la, "mod", "password123")
	if _, _, page := status(modWeb, "/admin"); !strings.Contains(page, `/admin/accounts/`+bob.ID+`/unsuspend`) || strings.Contains(page, `/admin/accounts/`+mod.ID+`/suspend`) {
		t.Error("admin user list lacks the right suspend buttons")
	}

	if err := la.UnsuspendAccount(bob); err != nil {
		t.Fatal(err)
	}
	eventually(t, "unsuspension Update delivered", func() bool { return len(r.receivedOf("Update")) == 2 })
	actor = ap.Object(r.receivedOf("Update")[1].Activity["object"])
	if _, ok := actor["suspended"]; ok || ap.Str(actor, "id") != bob.URI {
		t.Fatalf("Update object = %v", actor)
	}
	if code, _ := r.signedGet(st.URI, ""); code != 200 {
		t.Errorf("post = %d after lifting", code)
	}
	if _, path, _ := status(webClient(t, la, "bob", "password123"), "/home"); path != "/home" {
		t.Error("bob can't log in after the suspension was lifted")
	}
}

// A remote server suspending one of its accounts ("suspended": true in an
// actor Update, as Mastodon sends) hides it here until that server lifts it;
// suspensions by our own admins aren't lifted that way.
func TestRemoteOriginSuspension(t *testing.T) {
	la, r, _, alice := featureFixture(t)
	mod, _ := la.CreateLocalUser("mod", "password123", true)
	key := r.Base + "/users/alice#main-key"
	update := func(suspended bool, n string) {
		t.Helper()
		doc := mastodonActor
		if suspended {
			doc = strings.Replace(doc, `"memorial": false,`, `"memorial": false, "suspended": true,`, 1)
		}
		r.put("/users/alice", doc)
		var actor obj
		if err := json.Unmarshal([]byte(r.expand(doc)), &actor); err != nil {
			t.Fatal(err)
		}
		r.send(obj{"@context": asContext, "id": r.Base + "/users/alice#updates/" + n, "type": "Update", "actor": alice.URI,
			"to": []any{ap.Public}, "object": actor}, key, sigCavage)
	}
	suspended := func() bool {
		a, _ := la.AccountByID(alice.ID)
		return a != nil && a.Suspended
	}
	update(true, "1")
	eventually(t, "alice suspended by her server", suspended)
	if n := la.countInt(`SELECT COUNT(*) FROM follows WHERE account_id=? OR target_id=?`, alice.ID, alice.ID); n != 2 {
		t.Errorf("follows = %d; a suspension by the origin server shouldn't end them", n)
	}
	update(false, "2")
	eventually(t, "alice's suspension lifted by her server", func() bool { return !suspended() })

	a, _ := la.AccountByID(alice.ID)
	if err := la.AdminSuspend(mod, a); err != nil {
		t.Fatal(err)
	}
	update(false, "3")
	update(true, "4")
	update(false, "5")
	// the inbox drops these right away: alice is suspended and it wasn't her server's doing
	if !suspended() {
		t.Error("an Update from her server lifted our admin's suspension")
	}
}
