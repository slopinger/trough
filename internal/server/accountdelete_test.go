package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/slopinger/trough/internal/ap"
)

func TestDeleteOwnAccount(t *testing.T) {
	la, r, bob, _ := featureFixture(t)
	st, _ := la.PostStatus(context.Background(), bob, PostInput{Text: "bye", Visibility: "public"})
	la.FeatureTag(bob, "goodbye")

	c := webClient(t, la, "bob", "password123")
	page, _ := c.Get(la.base + "/settings")
	b, _ := io.ReadAll(page.Body)
	page.Body.Close()
	csrf := strings.SplitN(strings.SplitN(string(b), `name="csrf" value="`, 2)[1], `"`, 2)[0]
	del := func(form url.Values) {
		form.Set("csrf", csrf)
		req, _ := http.NewRequest("POST", la.base+"/settings/delete", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	del(url.Values{"confirm": {"bob"}, "password": {"wrong"}})
	if _, err := la.LocalAccount("bob"); err != nil {
		t.Fatal("deleted with a wrong password")
	}
	del(url.Values{"confirm": {"bob"}, "password": {"password123"}})
	if _, err := la.LocalAccount("bob"); err == nil {
		t.Fatal("account still there")
	}
	if _, err := la.StatusByURI(st.URI); err == nil {
		t.Fatal("post still there")
	}
	for _, q := range []string{`SELECT COUNT(*) FROM follows WHERE account_id=? OR target_id=?`, `SELECT COUNT(*) FROM sessions WHERE account_id=? OR account_id=?`,
		`SELECT COUNT(*) FROM featured_tags WHERE account_id=? OR account_id=?`} {
		if n := la.countInt(q, bob.ID, bob.ID); n != 0 {
			t.Fatalf("%s: %d left", q, n)
		}
	}
	eventually(t, "Delete of the actor delivered", func() bool {
		for _, rec := range r.receivedOf("Delete") {
			if ap.ID(rec.Activity["object"]) == bob.URI {
				return true
			}
		}
		return false
	})
	req, _ := http.NewRequest("GET", bob.URI, nil)
	req.Header.Set("Accept", "application/activity+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("actor: %d, want 410", resp.StatusCode)
	}
	if _, err := la.CreateLocalUser("bob", "password123", false); err == nil {
		t.Fatal("username could be taken again")
	}
	if _, err := la.checkPassword("bob", "password123"); err == nil {
		t.Fatal("deleted account can still log in")
	}
}
