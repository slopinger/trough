package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// featureFixture: local bob, remote Mastodon alice who follows bob (and is
// followed back), all over the live harness.
func featureFixture(t *testing.T) (*liveApp, *remote, *Account, *Account) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	r := newRemote(t, la)
	r.put("/users/alice", mastodonActor)
	alice, err := la.getAccount(context.Background(), r.Base+"/users/alice")
	if err != nil {
		t.Fatal(err)
	}
	la.db.Exec(`INSERT INTO follows (id,uri,account_id,target_id,accepted,created_at) VALUES ('f1',?,?,?,1,0)`, r.Base+"/follows/1", alice.ID, bob.ID)
	la.db.Exec(`INSERT INTO follows (id,uri,account_id,target_id,accepted,created_at) VALUES ('f2',?,?,?,1,0)`, bob.URI+"#follows/2", bob.ID, alice.ID)
	return la, r, bob, alice
}

func TestEditFederates(t *testing.T) {
	la, r, bob, _ := featureFixture(t)
	ctx := context.Background()
	st, _ := la.PostStatus(ctx, bob, PostInput{Text: "frist post", Visibility: "public"})
	full, _ := la.loadStatus(bob, st.ID)
	if _, err := la.EditStatus(ctx, bob, full, "first post #typo", "", false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Update delivered", func() bool { return len(r.receivedOf("Update")) == 1 })
	up := r.receivedOf("Update")[0]
	if up.SigErr != nil {
		t.Fatal(up.SigErr)
	}
	note := objOf(up.Activity["object"])
	if str(note, "id") != st.URI || !strings.Contains(str(note, "content"), "first post") || str(note, "updated") == "" {
		t.Fatalf("Update object = %v", note)
	}
	again, _ := la.loadStatus(bob, st.ID)
	if again.EditedAt.IsZero() || again.Source != "first post #typo" || len(again.Tags) != 1 {
		t.Fatalf("edit not stored: %+v", again)
	}
	alice := mustAccount(t, la, r.Base+"/users/alice")
	if _, err := la.EditStatus(ctx, alice, again, "hijack", "", false); err == nil {
		t.Fatal("edited someone else's post")
	}
}

func mustAccount(t *testing.T, la *liveApp, uri string) *Account {
	a, err := la.AccountByURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPinsFederate(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	ctx := context.Background()
	st, _ := la.PostStatus(ctx, bob, PostInput{Text: "pin me", Visibility: "public"})
	full, _ := la.loadStatus(bob, st.ID)
	if err := la.Pin(bob, full); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Add delivered", func() bool { return len(r.receivedOf("Add")) == 1 })
	add := r.receivedOf("Add")[0].Activity
	if idOf(add["object"]) != st.URI || idOf(add["target"]) != bob.URI+"/collections/featured" {
		t.Fatalf("Add = %v", add)
	}
	code, coll := r.signedGet(bob.URI+"/collections/featured", "")
	items, _ := coll["orderedItems"].([]any)
	if code != 200 || len(items) != 1 || str(objOf(items[0]), "id") != st.URI {
		t.Fatalf("featured = %d %v", code, coll)
	}
	priv, _ := la.PostStatus(ctx, bob, PostInput{Text: "secret", Visibility: "private"})
	privFull, _ := la.loadStatus(bob, priv.ID)
	if la.Pin(bob, privFull) == nil {
		t.Error("pinned a followers-only post")
	}
	full, _ = la.loadStatus(bob, st.ID)
	la.Unpin(bob, full)
	eventually(t, "Remove delivered", func() bool { return len(r.receivedOf("Remove")) == 1 })

	// a remote pin
	noteID := r.Base + "/users/alice/statuses/77"
	r.put("/users/alice/statuses/77", `{"id": "`+noteID+`", "type": "Note", "attributedTo": "{{R}}/users/alice", "content": "<p>pinned!</p>", "to": ["https://www.w3.org/ns/activitystreams#Public"], "published": "2026-01-01T00:00:00Z"}`)
	r.send(obj{"@context": asContext, "id": r.Base + "/add/1", "type": "Add", "actor": alice.URI, "object": noteID, "target": alice.URI + "/collections/featured"}, r.Base+"/users/alice#main-key", sigCavage)
	eventually(t, "remote pin stored", func() bool { s, err := la.StatusByURI(noteID); return err == nil && !s.PinnedAt.IsZero() })
	if p := la.pinnedStatuses(bob, alice); len(p) != 1 {
		t.Fatalf("remote pinned = %d", len(p))
	}
}

func TestBookmarksMutesFilters(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	ctx := context.Background()
	for _, txt := range []string{"<p>talking about politics again</p>", "<p>spoilers for the finale</p>", "<p>cute cats</p>"} {
		id := newID()
		la.insertStatus(&Status{ID: id, URI: r.Base + "/s/" + id, AccountID: alice.ID, Visibility: "public", Content: txt, CreatedAt: parseTime("2026-01-01T00:00:00Z")})
	}
	home := func() []*Status { s, _ := la.homeTimeline(bob, ""); return la.applyFilters(bob, s, "home") }
	if n := len(home()); n != 3 {
		t.Fatalf("home has %d posts", n)
	}
	la.AddFilter(bob, "politics", true, "hide")
	la.AddFilter(bob, "spoilers", true, "warn")
	sts := home()
	if len(sts) != 2 {
		t.Fatalf("hide filter: %d posts", len(sts))
	}
	warned := 0
	for _, s := range sts {
		if s.FilteredBy == "spoilers" {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("warn filter marked %d posts", warned)
	}
	if err := la.Bookmark(bob, sts[0]); err != nil {
		t.Fatal(err)
	}
	bm, _ := la.bookmarksTimeline(bob, "")
	if len(bm) != 1 || !bm[0].Bookmarked {
		t.Fatalf("bookmarks = %+v", bm)
	}
	la.Mute(bob, alice)
	if n := len(home()); n != 0 {
		t.Errorf("muted account still in home: %d", n)
	}
	if !la.relationship(bob.ID, alice.ID).Muting {
		t.Error("relationship doesn't show mute")
	}
	// muting is local: nothing is sent
	if n := len(r.receivedOf("Block")); n != 0 {
		t.Error("mute federated")
	}
	_ = ctx
}

func TestReportsFederate(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	admin, _ := la.CreateLocalUser("root2", "password123", true)
	id := newID()
	la.insertStatus(&Status{ID: id, URI: r.Base + "/s/bad", AccountID: alice.ID, Visibility: "public", Content: "<p>spam</p>", CreatedAt: parseTime("2026-01-01T00:00:00Z")})
	if _, err := la.FileReport(bob, alice, []string{id}, "this is spam", true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Flag delivered", func() bool { return len(r.receivedOf("Flag")) == 1 })
	flag := r.receivedOf("Flag")[0]
	if flag.SigErr != nil {
		t.Fatal(flag.SigErr)
	}
	objs := idsOf(flag.Activity["object"])
	if idOf(flag.Activity["actor"]) != la.instanceActor().URI || len(objs) != 2 || objs[0] != alice.URI || objs[1] != r.Base+"/s/bad" {
		t.Fatalf("Flag = %v", flag.Activity)
	}
	if strings.Contains(mustMarshal(flag.Activity), bob.URI) {
		t.Error("forwarded report reveals the reporter")
	}
	// they report bob
	mine, _ := la.PostStatus(context.Background(), bob, PostInput{Text: "hello", Visibility: "public"})
	r.send(obj{"@context": asContext, "id": r.Base + "/flags/1", "type": "Flag", "actor": alice.URI, "content": "rude", "object": []any{bob.URI, mine.URI}},
		r.Base+"/users/alice#main-key", sigCavage)
	eventually(t, "incoming report stored", func() bool { return len(la.openReports()) == 2 })
	if la.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=? AND type='report'`, admin.ID) != 2 {
		t.Error("admin not notified")
	}
}

func mustMarshal(v any) string { s, _ := jsonMarshal(v); return s }

func TestEditHistory(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	ctx := context.Background()
	st, _ := la.PostStatus(ctx, bob, PostInput{Text: "version one", Visibility: "public"})
	full, _ := la.loadStatus(bob, st.ID)
	la.EditStatus(ctx, bob, full, "version two", "", false)
	full, _ = la.loadStatus(bob, st.ID)
	la.EditStatus(ctx, bob, full, "version three", "cw", false)
	full, _ = la.loadStatus(bob, st.ID)
	h := la.history(full)
	if len(h) != 3 || !strings.Contains(h[0].Content, "version one") || !strings.Contains(h[2].Content, "version three") || h[2].Spoiler != "cw" {
		t.Fatalf("history = %+v", h)
	}
	token := oauthLogin(t, la, "bob", "password123", "read")
	c := &apiClient{t: t, base: la.base, token: token}
	if hist := c.ok("GET", "/api/v1/statuses/"+st.ID+"/history", nil).([]any); len(hist) != 3 {
		t.Fatalf("API history = %v", hist)
	}
	resp, err := http.Get(la.base + "/s/" + st.ID + "/history")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "version one") || !strings.Contains(string(body), "Edit 2") {
		t.Error("history page incomplete")
	}
	// a remote edit is recorded too
	nid := r.Base + "/users/alice/statuses/h"
	note := obj{"id": nid, "type": "Note", "attributedTo": alice.URI, "content": "<p>before</p>", "to": []any{asPublic}, "published": apTime(time.Now())}
	key := r.Base + "/users/alice#main-key"
	r.send(obj{"@context": asContext, "id": nid + "/a", "type": "Create", "actor": alice.URI, "to": []any{asPublic}, "object": note}, key, sigCavage)
	var rs *Status
	eventually(t, "remote post stored", func() bool { rs, _ = la.StatusByURI(nid); return rs != nil })
	note["content"] = "<p>after</p>"
	r.send(obj{"@context": asContext, "id": nid + "/u", "type": "Update", "actor": alice.URI, "object": note}, key, sigCavage)
	eventually(t, "remote edit in history", func() bool {
		s, _ := la.StatusByID(rs.ID)
		hh := la.history(s)
		return len(hh) == 2 && strings.Contains(hh[0].Content, "before")
	})
	// poll count refreshes are not edits
	poll, _ := newPoll([]string{"a", "b"}, "1d", false)
	ps, _ := la.PostStatus(ctx, bob, PostInput{Text: "poll", Visibility: "public", Poll: poll})
	ps.Poll.Options[0].Votes = 3
	la.updateStatusContent(ps)
	if n := la.countInt(`SELECT COUNT(*) FROM status_edits WHERE status_id=?`, ps.ID); n != 0 {
		t.Errorf("poll refresh recorded %d edits", n)
	}
}

// Clients (Tusky, Mastodon apps) send the post language; we keep it and
// federate it as contentMap, like Mastodon.
func TestPostLanguage(t *testing.T) {
	la, r, _, _ := featureFixture(t)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	st := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"Guten Morgen"}, "language": {"de"}}).(map[string]any)
	if st["language"] != "de" {
		t.Fatalf("language = %v", st["language"])
	}
	eventually(t, "Create delivered", func() bool { return len(r.receivedOf("Create")) == 1 })
	note := objOf(r.receivedOf("Create")[0].Activity["object"])
	if cm := objOf(note["contentMap"]); cm == nil || !strings.Contains(str(cm, "de"), "Guten Morgen") {
		t.Errorf("contentMap = %v", note["contentMap"])
	}
	if st := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"x"}, "language": {"<script>"}}).(map[string]any); st["language"] != nil {
		t.Errorf("bad language kept: %v", st["language"])
	}
}

func TestAccountNotes(t *testing.T) {
	la, _, bob, alice := featureFixture(t)
	la.CreateLocalUser("carol", "password123", false)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write follow")}
	rel := c.ok("POST", "/api/v1/accounts/"+alice.ID+"/note", url.Values{"comment": {"met at the <bakery>"}}).(map[string]any)
	if rel["note"] != "met at the <bakery>" {
		t.Fatalf("relationship = %v", rel)
	}
	if rels := c.ok("GET", "/api/v1/accounts/relationships?id[]="+alice.ID, nil).([]any); rels[0].(map[string]any)["note"] != "met at the <bakery>" {
		t.Errorf("relationships = %v", rels)
	}
	// only the author sees it
	carol := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "carol", "password123", "read")}
	if rels := carol.ok("GET", "/api/v1/accounts/relationships?id[]="+alice.ID, nil).([]any); rels[0].(map[string]any)["note"] != "" {
		t.Errorf("carol sees bob's note: %v", rels)
	}
	if code, _, _ := c.do("POST", "/api/v1/accounts/"+alice.ID+"/note", url.Values{"comment": {strings.Repeat("x", 2001)}}); code != 422 {
		t.Errorf("overlong note: %d", code)
	}
	c.ok("POST", "/api/v1/accounts/"+alice.ID+"/note", url.Values{"comment": {""}})
	if la.accountNote(bob, alice) != "" {
		t.Error("empty comment didn't clear the note")
	}
}
