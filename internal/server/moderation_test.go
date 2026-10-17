package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/slopinger/trough/internal/ap"
)

func TestReportModeration(t *testing.T) {
	la, r, bob, alice := featureFixture(t) // alice (remote) and bob (local) follow each other
	ctx := context.Background()
	root, err := la.CreateLocalUser("mod", "password123", true)
	if err != nil {
		t.Fatal(err)
	}
	key := r.Base + "/users/alice#main-key"

	// bob reports a post by alice
	nid := r.Base + "/users/alice/statuses/bad"
	r.send(obj{"@context": asContext, "id": nid + "/a", "type": "Create", "actor": alice.URI, "to": []any{ap.Public},
		"object": obj{"id": nid, "type": "Note", "attributedTo": alice.URI, "content": "<p>something nasty</p>", "to": []any{ap.Public}, "published": ap.Time(time.Now())}}, key, sigCavage)
	var bad *Status
	eventually(t, "remote post stored", func() bool { bad, _ = la.StatusByURI(nid); return bad != nil })
	aboutAlice, err := la.FileReport(bob, alice, []string{bad.ID}, "this is spam", false)
	if err != nil {
		t.Fatal(err)
	}
	// alice's server reports a post by bob
	mine, _ := la.PostStatus(ctx, bob, PostInput{Text: "a post someone dislikes", Visibility: "public"})
	r.send(obj{"@context": asContext, "id": r.Base + "/flags/1", "type": "Flag", "actor": alice.URI, "content": "rude",
		"object": []any{bob.URI, mine.URI}}, key, sigCavage)
	var aboutBob string
	eventually(t, "remote report stored", func() bool {
		la.db.QueryRow(`SELECT id FROM reports WHERE target_id=?`, bob.ID).Scan(&aboutBob)
		return aboutBob != ""
	})

	web := webClient(t, la, "mod", "password123")
	get := func(path string) string {
		t.Helper()
		resp, err := web.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	page := get("/admin/reports")
	for _, want := range []string{"this is spam", "something nasty", "rude", "a post someone dislikes", "Suspend account", "(2)"} {
		if !strings.Contains(page, want) {
			t.Errorf("reports page lacks %q", want)
		}
	}
	if !strings.Contains(get("/admin"), "2 open reports") {
		t.Error("admin page doesn't summarize open reports")
	}

	// deleting a reported local post federates the deletion
	if msg, err := la.ResolveReport(root, aboutBob, "delete", "agreed, removed"); err != nil || !strings.Contains(msg, "1 post") {
		t.Fatalf("delete = %q %v", msg, err)
	}
	if st, _ := la.StatusByID(mine.ID); st != nil {
		t.Error("reported post still there")
	}
	eventually(t, "Delete federated", func() bool { return len(r.receivedOf("Delete")) >= 1 })
	if _, err := la.ResolveReport(root, aboutBob, "none", ""); err == nil {
		t.Error("resolved a report twice")
	}

	// suspending through the web form, with a note
	csrf := strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0]
	req, _ := http.NewRequest("POST", la.base+"/admin/reports/"+aboutAlice, strings.NewReader(url.Values{"csrf": {csrf}, "action": {"suspend"}, "note": {"spam account"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", la.base)
	resp, err := web.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	a, _ := la.AccountByID(alice.ID)
	if !a.Suspended || la.relationship(bob.ID, alice.ID).Following || la.relationship(alice.ID, bob.ID).Following {
		t.Fatalf("suspend: suspended=%v follows remain=%v", a.Suspended, la.relationship(bob.ID, alice.ID).Following)
	}
	if sts, _ := la.queryStatuses(bob, `s.id=?`, []any{bad.ID}, "", 1, false); len(sts) != 0 {
		t.Error("suspended account's post still shown")
	}
	// refetching the actor doesn't undo the suspension
	a.Suspended = false
	la.saveAccount(a)
	if a2, _ := la.AccountByID(alice.ID); !a2.Suspended {
		t.Error("re-saving the actor lifted the suspension")
	}

	resolved := get("/admin/reports?tab=resolved")
	for _, want := range []string{"Suspended the account", "spam account", "Deleted the reported posts", "agreed, removed", "@mod", "Lift suspension"} {
		if !strings.Contains(resolved, want) {
			t.Errorf("resolved tab lacks %q", want)
		}
	}
	if !strings.Contains(get("/admin/reports"), "No open reports") {
		t.Error("open tab not empty")
	}

	if err := la.UnsuspendAccount(a); err != nil {
		t.Fatal(err)
	}
	if a3, _ := la.AccountByID(alice.ID); a3.Suspended {
		t.Error("suspension not lifted")
	}

	// only admins get the page
	plain := webClient(t, la, "bob", "password123")
	if resp, err := plain.Get(la.base + "/admin/reports"); err != nil || (resp.StatusCode == 200 && resp.Request.URL.Path == "/admin/reports") {
		t.Errorf("non-admin got the reports page")
	}
}
