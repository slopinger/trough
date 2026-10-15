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

	"git.freifick.net/slop/trough/internal/ap"
	"git.freifick.net/slop/trough/internal/ulid"
)

const relayActor = `{"@context": ["https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1"],
 "id": "{{R}}/actor", "type": "Application", "preferredUsername": "relay", "inbox": "{{R}}/inbox",
 "publicKey": {"id": "{{R}}/actor#main-key", "owner": "{{R}}/actor", "publicKeyPem": "{{PEM}}"}}`

func TestRelays(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	r := newRemote(t, la)
	r.put("/actor", relayActor)
	r.put("/users/carol", strings.NewReplacer("users/alice", "users/carol", `"preferredUsername": "alice"`, `"preferredUsername": "carol"`).Replace(mastodonActor))
	relayKey := r.Base + "/actor#main-key"

	// Mastodon-style relay: subscribe via its inbox
	if err := la.AddRelay(context.Background(), r.Base+"/inbox"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "relay Follow delivered", func() bool { return len(r.receivedOf("Follow")) == 1 })
	f := r.receivedOf("Follow")[0]
	if f.SigErr != nil || ap.ID(f.Activity["object"]) != ap.Public || ap.ID(f.Activity["actor"]) != la.instanceActor().URI {
		t.Fatalf("relay follow = %v (%v)", f.Activity, f.SigErr)
	}
	r.send(obj{"@context": asContext, "id": r.Base + "/accepts/1", "type": "Accept", "actor": r.Base + "/actor", "object": f.Activity}, relayKey, sigCavage)
	eventually(t, "relay accepted", func() bool { rs := la.relays(); return len(rs) == 1 && rs[0].Accepted })

	// the relay forwards carol's Create (signed by the relay, actor carol)
	noteID := r.Base + "/users/carol/statuses/1"
	note := `{"id": "` + noteID + `", "type": "Note", "attributedTo": "{{R}}/users/carol", "content": "<p>relayed!</p>", "to": ["https://www.w3.org/ns/activitystreams#Public"], "published": "2026-01-01T00:00:00Z"}`
	r.put("/users/carol/statuses/1", note)
	r.send(obj{"@context": asContext, "id": noteID + "/activity", "type": "Create", "actor": r.Base + "/users/carol",
		"to": []any{ap.Public}, "object": mustJSON(t, r.expand(note))}, relayKey, sigCavage)
	eventually(t, "relayed post stored", func() bool { _, err := la.StatusByURI(noteID); return err == nil })
	fed, _ := la.federatedTimeline(bob, "")
	if len(fed) != 1 {
		t.Fatalf("federated timeline has %d posts", len(fed))
	}
	// LitePub-style Announce from the relay
	note2ID := r.Base + "/users/carol/statuses/2"
	r.put("/users/carol/statuses/2", strings.Replace(note, noteID, note2ID, 1))
	r.send(obj{"@context": asContext, "id": r.Base + "/announces/1", "type": "Announce", "actor": r.Base + "/actor", "to": []any{ap.Public}, "object": note2ID}, relayKey, sigCavage)
	eventually(t, "announced post stored", func() bool { _, err := la.StatusByURI(note2ID); return err == nil })
	if la.countInt(`SELECT COUNT(*) FROM statuses WHERE boost_of_id != ''`) != 0 {
		t.Error("relay announce stored as a boost")
	}
	// our public posts go to the relay
	la.PostStatus(context.Background(), bob, PostInput{Text: "hello relay", Visibility: "public"})
	la.PostStatus(context.Background(), bob, PostInput{Text: "not for the relay", Visibility: "unlisted"})
	eventually(t, "public post sent to relay", func() bool { return len(r.receivedOf("Create")) >= 1 })
	time.Sleep(300 * time.Millisecond)
	if n := len(r.receivedOf("Create")); n != 1 {
		t.Errorf("relay got %d Creates, want 1 (public only)", n)
	}

	// someone who isn't a relay can't forward other people's activities
	r2 := newRemote(t, la)
	r2.put("/users/mallory", strings.NewReplacer("users/alice", "users/mallory", `"preferredUsername": "alice"`, `"preferredUsername": "mallory"`).Replace(mastodonActor))
	if code := r2.send(obj{"@context": asContext, "id": r.Base + "/forged", "type": "Create", "actor": r.Base + "/users/carol",
		"object": obj{"id": r.Base + "/users/carol/statuses/3", "type": "Note", "attributedTo": r.Base + "/users/carol", "content": "forged"}},
		r2.Base+"/users/mallory#main-key", sigCavage); code != 401 {
		t.Errorf("forwarded activity from a non-relay: %d, want 401", code)
	}
	// unsubscribe
	la.RemoveRelay(la.relays()[0].ID)
	eventually(t, "Undo sent to relay", func() bool { return len(r.receivedOf("Undo")) == 1 })
}

func TestFollowedTagsAndLists(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	r.put("/users/carol", strings.NewReplacer("users/alice", "users/carol", `"preferredUsername": "alice"`, `"preferredUsername": "carol"`).Replace(mastodonActor))
	carol, err := la.getAccount(context.Background(), r.Base+"/users/carol")
	if err != nil {
		t.Fatal(err)
	}
	add := func(acc *Account, html string, tags ...string) *Status {
		st := &Status{ID: ulid.New(), URI: r.Base + "/s/" + ulid.New(), AccountID: acc.ID, Visibility: "public", Content: html, CreatedAt: time.Now()}
		la.insertStatus(st)
		for _, tg := range tags {
			la.db.Exec(`INSERT INTO status_tags (status_id, tag) VALUES (?,?)`, st.ID, tg)
		}
		return st
	}
	catPost := add(carol, "<p>#cats</p>", "cats")
	add(carol, "<p>no tags</p>")
	alicePost := add(alice, "<p>alice says hi</p>")
	homeIDs := func() map[string]bool {
		sts, _ := la.homeTimeline(bob, "")
		m := map[string]bool{}
		for _, s := range sts {
			m[s.ID] = true
		}
		return m
	}
	if homeIDs()[catPost.ID] {
		t.Fatal("unfollowed tag in home")
	}
	la.FollowTag(bob, "#Cats")
	if h := homeIDs(); !h[catPost.ID] || len(h) != 2 {
		t.Fatalf("followed tag: home = %v", h)
	}
	// lists via the API
	token := oauthLogin(t, la, "bob", "password123", "read write follow")
	c := &apiClient{t: t, base: la.base, token: token}
	if tags := c.ok("GET", "/api/v1/followed_tags", nil).([]any); len(tags) != 1 || tags[0].(map[string]any)["name"] != "cats" {
		t.Fatalf("followed_tags = %v", tags)
	}
	l := c.ok("POST", "/api/v1/lists", url.Values{"title": {"Friends"}, "exclusive": {"true"}}).(map[string]any)
	id := l["id"].(string)
	if code, _, _ := c.do("POST", "/api/v1/lists/"+id+"/accounts", url.Values{"account_ids[]": {carol.ID}}); code != 422 {
		t.Errorf("added an account bob doesn't follow: %d", code)
	}
	c.ok("POST", "/api/v1/lists/"+id+"/accounts", url.Values{"account_ids[]": {alice.ID}})
	tl := c.ok("GET", "/api/v1/timelines/list/"+id, nil).([]any)
	if len(tl) != 1 || tl[0].(map[string]any)["id"] != alicePost.ID {
		t.Fatalf("list timeline = %v", tl)
	}
	if homeIDs()[alicePost.ID] {
		t.Error("exclusive list member still in home")
	}
	if ls := c.ok("GET", "/api/v1/accounts/"+alice.ID+"/lists", nil).([]any); len(ls) != 1 {
		t.Errorf("account lists = %v", ls)
	}
	c.ok("DELETE", "/api/v1/lists/"+id, nil)
	if !homeIDs()[alicePost.ID] {
		t.Error("post not back in home after deleting the list")
	}
}

// fedi.buzz-style tag relays are added by their actor URL through the admin
// page. Several share one inbox: each gets its own Follow, the page lists the
// actors, and our public posts reach the shared inbox once.
func TestLitePubRelayViaAdmin(t *testing.T) {
	la := startLiveApp(t)
	boss, _ := la.CreateLocalUser("boss", "password123", true)
	r := newRemote(t, la)
	for _, tag := range []string{"cats", "dogs"} {
		r.put("/tag/"+tag, strings.NewReplacer(`"{{R}}/actor"`, `"{{R}}/tag/`+tag+`"`, `"{{R}}/actor#main-key"`, `"{{R}}/tag/`+tag+`#main-key"`, `"type": "Application"`, `"type": "Service"`,
			`"inbox": "{{R}}/inbox"`, `"inbox": "{{R}}/tag/`+tag+`", "endpoints": {"sharedInbox": "{{R}}/instance/relay"}`).Replace(relayActor))
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	post := func(path string, form url.Values) string {
		req, _ := http.NewRequest("POST", la.base+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	post("/login", url.Values{"username": {"boss"}, "password": {"password123"}})
	resp, _ := c.Get(la.base + "/admin")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := strings.SplitN(strings.SplitN(string(page), `name="csrf" value="`, 2)[1], `"`, 2)[0]
	post("/admin/relays", url.Values{"csrf": {csrf}, "url": {r.Base + "/tag/cats"}})
	page2 := post("/admin/relays", url.Values{"csrf": {csrf}, "url": {r.Base + "/tag/dogs"}})
	eventually(t, "Follows delivered", func() bool { return len(r.receivedOf("Follow")) == 2 })
	for i, tag := range []string{"cats", "dogs"} {
		f := r.receivedOf("Follow")[i]
		if f.SigErr != nil || ap.ID(f.Activity["object"]) != r.Base+"/tag/"+tag || f.Path != "/instance/relay" {
			t.Fatalf("follow to %s = %v (%v)", f.Path, f.Activity, f.SigErr)
		}
		if !strings.Contains(page2, r.Base+"/tag/"+tag+"</td>") {
			t.Errorf("admin page doesn't list the %s relay", tag)
		}
		key := r.Base + "/tag/" + tag + "#main-key"
		r.send(obj{"@context": asContext, "id": r.Base + "/accepts/" + tag, "type": "Accept", "actor": r.Base + "/tag/" + tag, "object": f.Activity["id"]}, key, sigCavage)
	}
	eventually(t, "both relays accepted", func() bool { rs := la.relays(); return len(rs) == 2 && rs[0].Accepted && rs[1].Accepted })
	la.PostStatus(context.Background(), boss, PostInput{Text: "hello #cats and #dogs", Visibility: "public"})
	eventually(t, "post sent to the relay", func() bool { return len(r.receivedOf("Create")) >= 1 })
	time.Sleep(300 * time.Millisecond)
	if n := len(r.receivedOf("Create")); n != 1 {
		t.Errorf("shared relay inbox got the post %d times", n)
	}
}
