package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAdminAccountPage(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	la.CreateLocalUser("boss", "password123", true)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	spam, _ := la.CreateLocalUser("spam", "password123", false)
	la.FileReport(bob, spam, nil, "spammy bio", false)

	admin := webClient(t, la, "boss", "password123")
	get := func(c *http.Client, path string) (int, string) {
		t.Helper()
		resp, err := c.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	post := func(path string, form url.Values) string {
		t.Helper()
		_, page := get(admin, "/admin/accounts/"+spam.ID)
		form.Set("csrf", strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0])
		req, _ := http.NewRequest("POST", la.base+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	_, page := get(admin, "/admin/accounts/"+spam.ID)
	if !strings.Contains(page, "spammy bio") || !strings.Contains(page, `value="limit"`) {
		t.Fatal("page lacks the report or the actions")
	}
	if code, _ := get(webClient(t, la, "bob", "password123"), "/admin/accounts/"+spam.ID); code == 200 {
		t.Error("a regular user opened the admin page")
	}

	// notes
	post("/admin/accounts/"+spam.ID+"/notes", url.Values{"content": {"Talked to them on matrix."}})
	if notes := la.moderationNotes(spam.ID); len(notes) != 1 || notes[0].Author.Username != "boss" {
		t.Fatalf("notes: %+v", notes)
	}

	// limiting directly warns them with the message
	post("/admin/accounts/"+spam.ID+"/action", url.Values{"action": {"limit"}, "message": {"Tone it down."}})
	if spam, _ = la.AccountByID(spam.ID); !spam.Limited {
		t.Fatal("not limited")
	}
	ws := la.loadWarnings(`w.account_id=?`, spam.ID)
	if len(ws) != 1 || ws[0].Action != "silence" || ws[0].Text != "Tone it down." {
		t.Fatalf("warnings: %+v", ws)
	}
	if page := post("/admin/accounts/"+spam.ID+"/action", url.Values{"action": {"unlimit"}}); !strings.Contains(page, "Lifted the limit") {
		t.Error("unlimit flash missing")
	}
	if len(la.loadWarnings(`w.account_id=?`, spam.ID)) != 1 {
		t.Error("lifting the limit warned them")
	}

	// remote accounts: no warnings, but suspending works
	r := newRemote(t, la)
	r.put("/users/alice", mastodonActor)
	alice, _ := la.getAccount(ctx, r.Base+"/users/alice")
	if _, err := la.moderateAccount(spam, alice, "warn", ""); err == nil {
		t.Error("warned a remote account")
	}
	boss, _ := la.LocalAccount("boss")
	if _, err := la.moderateAccount(boss, alice, "suspend", "ignored"); err != nil {
		t.Fatal(err)
	}
	if alice, _ = la.AccountByID(alice.ID); !alice.Suspended {
		t.Error("remote account not suspended")
	}
	if _, err := la.moderateAccount(boss, boss, "suspend", ""); err == nil {
		t.Error("an admin suspended themselves")
	}
}
