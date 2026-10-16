package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFieldLink(t *testing.T) {
	for value, want := range map[string]string{
		fieldHTML("https://pig.example/"):                                      "https://pig.example/",
		`<a href="https://pig.example/me">pig.example/me</a>`:                  "https://pig.example/me",
		`<a href="https://pig.example/">site</a> and more text`:                "",
		`<a href="https://a.example/">a</a><a href="https://b.example/">b</a>`: "",
		`<a href="javascript:alert(1)">x</a>`:                                  "",
		"just text":                                                            "",
	} {
		if got := fieldLink(value); got != want {
			t.Errorf("fieldLink(%q) = %q, want %q", value, got, want)
		}
	}
	if !strings.Contains(fieldHTML("https://pig.example/"), `rel="me nofollow`) {
		t.Error("our own field links lack rel=me")
	}
}

func TestRelMeLinks(t *testing.T) {
	base, _ := url.Parse("https://pig.example/about/")
	page := `<html><head><link rel="me" href="https://fedi.example/@pig"></head><body>
		<a href="/x">plain</a><a rel="nofollow ME" href="../users/pig">rel me</a><a rel="menu" href="https://no.example/">menu</a></body></html>`
	got, _ := relMeLinks(strings.NewReader(page), base)
	if len(got) != 2 || got[0] != "https://fedi.example/@pig" || got[1] != "https://pig.example/users/pig" {
		t.Fatalf("rel=me links = %q", got)
	}
	if normalizeMeURL("HTTPS://Fedi.Example/@pig/#top") != normalizeMeURL("https://fedi.example/@pig") {
		t.Error("normalizeMeURL is too strict")
	}
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestProfileLinkVerification(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	var hits atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/bob":
			io.WriteString(w, `<a rel="me" href="`+la.base+`/@bob">me on the fediverse</a>`)
		case "/remote":
			io.WriteString(w, `<link rel="me" href="https://remote.example/@carl/">`)
		default:
			io.WriteString(w, `<a href="`+la.base+`/@bob">no rel=me here</a>`)
		}
	}))
	defer site.Close()

	bob.Fields = []Field{{Name: "Site", Value: fieldHTML(site.URL + "/bob")}, {Name: "Other", Value: fieldHTML(site.URL + "/other")}}
	if err := la.UpdateProfile(bob); err != nil {
		t.Fatal(err)
	}
	fields := func(id string) []Field {
		acc, err := la.AccountByID(id)
		if err != nil {
			t.Fatal(err)
		}
		return acc.Fields
	}
	waitFor(t, "verification", func() bool { return fields(bob.ID)[0].VerifiedAt != 0 })
	if f := fields(bob.ID); f[1].VerifiedAt != 0 {
		t.Error("a page without rel=me verified the link")
	}
	c := &apiClient{t: t, base: la.base}
	acct := c.ok("GET", "/api/v1/accounts/"+bob.ID, nil).(map[string]any)
	af := acct["fields"].([]any)
	if af[0].(map[string]any)["verified_at"] == nil || af[1].(map[string]any)["verified_at"] != nil {
		t.Errorf("API fields = %v", af)
	}
	resp, err := http.Get(la.base + "/@bob")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Count(string(body), `class="verified"`) != 1 {
		t.Error("profile page doesn't mark exactly one field verified")
	}

	// Saving again keeps the check; changing the value drops it.
	bob, _ = la.AccountByID(bob.ID)
	la.UpdateProfile(bob)
	if fields(bob.ID)[0].VerifiedAt == 0 {
		t.Error("unchanged field lost its verification")
	}
	bob.Fields[0].Value = fieldHTML(site.URL + "/other")
	la.UpdateProfile(bob)
	if fields(bob.ID)[0].VerifiedAt != 0 {
		t.Error("changed field kept its verification")
	}

	// Remote actors are checked against their own profile URL, once per new value.
	actor := obj{"id": "https://remote.example/users/carl", "type": "Person", "preferredUsername": "carl",
		"url": "https://remote.example/@carl", "inbox": "https://remote.example/users/carl/inbox",
		"attachment": []any{obj{"type": "PropertyValue", "name": "Web", "value": `<a href="` + site.URL + `/remote" rel="me">site</a>`}}}
	carl, err := la.upsertActor(actor)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "remote verification", func() bool { return fields(carl.ID)[0].VerifiedAt != 0 })
	before := hits.Load()
	if _, err := la.upsertActor(actor); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if hits.Load() != before || fields(carl.ID)[0].VerifiedAt == 0 {
		t.Error("refetching an unchanged actor refetched its site or lost the verification")
	}

	// An actor claiming another server's profile URL can't borrow its links.
	mallory := obj{"id": "https://evil.example/users/mallory", "type": "Person", "preferredUsername": "mallory",
		"url": "https://remote.example/@carl", "inbox": "https://evil.example/inbox",
		"attachment": []any{obj{"type": "PropertyValue", "name": "Web", "value": `<a href="` + site.URL + `/remote">site</a>`}}}
	m, err := la.upsertActor(mallory)
	if err != nil {
		t.Fatal(err)
	}
	before = hits.Load()
	waitFor(t, "the site fetch", func() bool { return hits.Load() > before || fields(m.ID)[0].VerifiedAt != 0 })
	time.Sleep(200 * time.Millisecond)
	if fields(m.ID)[0].VerifiedAt != 0 {
		t.Error("foreign profile url verified a link")
	}
}
