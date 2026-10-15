package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

func TestCSVExportImport(t *testing.T) {
	la, _, bob, alice := featureFixture(t)
	eve, _ := la.CreateLocalUser("eve", "password123", false)
	la.Mute(bob, eve)
	la.BlockDomain(bob, "spam.example")
	l, _ := la.CreateList(bob, "friends", "list", false)
	la.AddToList(bob, l, alice)

	bc := webClient(t, la, "bob", "password123")
	download := func(c *http.Client, file string) string {
		resp, err := c.Get(la.base + "/settings/export/" + file)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), file) {
			t.Fatalf("%s: %d", file, resp.StatusCode)
		}
		return string(b)
	}
	following := download(bc, "following_accounts.csv")
	if !strings.HasPrefix(following, "Account address,Show boosts") || !strings.Contains(following, alice.Acct()+",true,false,") {
		t.Fatalf("following_accounts.csv = %q", following)
	}
	if got := download(bc, "muted_accounts.csv"); !strings.Contains(got, "eve@"+la.cfg.Host+",true") {
		t.Fatalf("muted_accounts.csv = %q", got)
	}
	if got := download(bc, "blocked_domains.csv"); got != "spam.example\n" {
		t.Fatalf("blocked_domains.csv = %q", got)
	}
	lists := download(bc, "lists.csv")
	if lists != "friends,"+alice.Acct()+"\n" {
		t.Fatalf("lists.csv = %q", lists)
	}

	ec := webClient(t, la, "eve", "password123")
	page, _ := ec.Get(la.base + "/settings")
	b, _ := io.ReadAll(page.Body)
	page.Body.Close()
	csrf := strings.SplitN(strings.SplitN(string(b), `name="csrf" value="`, 2)[1], `"`, 2)[0]
	upload := func(kind, content string, wantFail int) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("csrf", csrf)
		mw.WriteField("type", kind)
		fw, _ := mw.CreateFormFile("file", kind+".csv")
		fw.Write([]byte(content))
		mw.Close()
		req, _ := http.NewRequest("POST", la.base+"/settings/import", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Origin", la.base)
		resp, err := ec.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		eventually(t, kind+" import", func() bool { s := importStatus(eve.ID); return s != nil && !s.Running })
		if s := importStatus(eve.ID); s.Total < 1 || s.Fail != wantFail {
			t.Fatalf("%s import: %+v", kind, s)
		}
	}
	upload("following", following+"nobody@"+la.cfg.Host+",true,false,\n", 1) // no such account
	upload("domain_blocks", "Spam.Example\n", 0)
	upload("lists", lists, 0)
	if r := la.relationship(eve.ID, alice.ID); !r.Following && !r.Requested {
		t.Fatal("follow not imported")
	}
	if !la.blocksDomain(eve.ID, "spam.example") {
		t.Fatal("domain block not imported")
	}
	if got := la.lists(eve); len(got) != 1 || got[0].Title != "friends" {
		t.Fatalf("lists = %+v", got)
	}
}
