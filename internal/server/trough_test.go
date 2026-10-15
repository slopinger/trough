package server

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.freifick.net/slop/trough/internal/httpsig"
)

func testApp(t *testing.T) *App {
	t.Helper()
	app, err := NewApp(&Config{Host: "example.test", Scheme: "https", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

func TestSignatureRoundTrip(t *testing.T) {
	priv, pub, err := httpsig.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := httpsig.ParsePrivateKey(priv)
	body := []byte(`{"type":"Follow"}`)
	req, _ := http.NewRequest("POST", "https://remote.test/users/x/inbox?a=1", bytes.NewReader(body))
	if err := httpsig.SignCavage(req, body, "https://me.test/users/me#main-key", key); err != nil {
		t.Fatal(err)
	}
	// simulate the server side
	in := httptest.NewRequest("POST", "/users/x/inbox?a=1", bytes.NewReader(body))
	in.Host = "remote.test"
	in.Header = req.Header.Clone()
	p, err := httpsig.ParseCavage(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.KeyID != "https://me.test/users/me#main-key" {
		t.Fatalf("keyId = %q", p.KeyID)
	}
	ss, err := httpsig.CheckCavage(in, p, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := httpsig.VerifyRSA(pub, ss, p.Signature, p.Algorithm); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// tampered body must fail the digest check
	if _, err := httpsig.CheckCavage(in, p, []byte(`{"type":"Delete"}`)); err == nil {
		t.Fatal("tampered body accepted")
	}
	// a different path must fail verification
	in2 := httptest.NewRequest("POST", "/inbox", bytes.NewReader(body))
	in2.Host = "remote.test"
	in2.Header = req.Header.Clone()
	ss2, err := httpsig.CheckCavage(in2, p, body)
	if err != nil {
		t.Fatal(err)
	}
	if httpsig.VerifyRSA(pub, ss2, p.Signature, p.Algorithm) == nil {
		t.Fatal("signature for another path accepted")
	}
	// stale date must be rejected
	in.Header.Set("Date", "Mon, 02 Jan 2006 15:04:05 GMT")
	if _, err := httpsig.CheckCavage(in, p, body); err == nil {
		t.Fatal("stale date accepted")
	}
}

func TestVisibilityFrom(t *testing.T) {
	f := "https://r.test/users/a/followers"
	cases := []struct {
		m    obj
		want string
	}{
		{obj{"to": []any{asPublic}, "cc": []any{f}}, "public"},
		{obj{"to": []any{f}, "cc": []any{asPublic}}, "unlisted"},
		{obj{"to": []any{f}}, "private"},
		{obj{"to": []any{"https://x.test/users/b"}}, "direct"},
		{obj{"to": "as:Public"}, "public"},
	}
	for _, c := range cases {
		if got := visibilityFrom(c.m, f); got != c.want {
			t.Errorf("visibilityFrom(%v) = %s, want %s", c.m, got, c.want)
		}
	}
}

func TestSanitize(t *testing.T) {
	in := `<p onclick="x()">hi<script>alert(1)</script> <a href="javascript:alert(1)">x</a> <a href="https://ok.test/" class="mention">@ok</a><img src=x onerror=alert(1)></p>`
	out := sanitizeHTML(in)
	for _, bad := range []string{"script", "onclick", "javascript:", "<img", "onerror"} {
		if strings.Contains(out, bad) {
			t.Errorf("sanitized output contains %q: %s", bad, out)
		}
	}
	if !strings.Contains(out, `href="https://ok.test/"`) || !strings.Contains(out, `class="mention"`) {
		t.Errorf("safe link/class removed: %s", out)
	}
}

func TestPublicIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.1", "169.254.169.254", "::1", "fe80::1", "100.64.0.1", "0.0.0.0", "fc00::1"} {
		if isPublicIP(net.ParseIP(s)) {
			t.Errorf("%s treated as public", s)
		}
	}
	for _, s := range []string{"45.150.123.24", "1.1.1.1", "2a0f:be01:0:100::113"} {
		if !isPublicIP(net.ParseIP(s)) {
			t.Errorf("%s treated as private", s)
		}
	}
}

func TestFormatPost(t *testing.T) {
	app := testApp(t)
	acc, err := app.CreateLocalUser("bob", "password123", false)
	if err != nil {
		t.Fatal(err)
	}
	f := app.formatPost(context.Background(), "hi @bob and @nobody, see https://example.com/x. #Go #123\n\n<b>not bold</b>")
	if len(f.Mentions) != 1 || f.Mentions[0].ID != acc.ID {
		t.Fatalf("mentions = %v", f.Mentions)
	}
	if len(f.Tags) != 1 || f.Tags[0] != "go" {
		t.Fatalf("tags = %v", f.Tags)
	}
	for _, want := range []string{`class="u-url mention"`, `href="https://example.com/x"`, `/tags/go`, "&lt;b&gt;not bold&lt;/b&gt;", "@nobody", "</p><p>"} {
		if !strings.Contains(f.HTML, want) {
			t.Errorf("formatted HTML lacks %q: %s", want, f.HTML)
		}
	}
	if strings.Contains(f.HTML, "x.</span>") {
		t.Errorf("trailing period was linked: %s", f.HTML)
	}
}

func TestRenderRewritesMentions(t *testing.T) {
	app := testApp(t)
	m := &Account{Username: "al", Domain: "r.test", URI: "https://r.test/users/al", URL: "https://r.test/@al"}
	out := string(app.renderHTML(`<p><a href="https://r.test/@al" class="u-url mention">@al</a> <a href="https://r.test/tags/Cats" class="mention hashtag">#Cats</a> <a href="https://elsewhere.test/">x</a> :blob:</p>`,
		[]*Account{m}, []Emoji{{Shortcode: "blob", URL: "https://r.test/blob.png"}}))
	for _, want := range []string{`href="/@al@r.test"`, `href="/tags/cats"`, `target="_blank"`, `class="emoji"`, `/proxy/`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered HTML lacks %q: %s", want, out)
		}
	}
}

func TestULIDOrder(t *testing.T) {
	a, b := newID(), newID()
	if len(a) != 26 || len(b) != 26 {
		t.Fatal("bad id length")
	}
	old := newIDAt(parseTime("2020-01-01T00:00:00Z"))
	if !(old < a) {
		t.Fatal("ids do not sort by time")
	}
}

// Mentions of accounts we never fetched must still link to our own profile
// pages, and replies must tag everyone who was mentioned.
func TestUnknownMentions(t *testing.T) {
	app := testApp(t)
	me, _ := app.CreateLocalUser("me", "password123", false)
	content := `<p><span class="h-card"><a href="https://remote.test/@alice" class="u-url mention">@<span>alice</span></a></span> ` +
		`<a href="https://other.test/users/carol" class="u-url mention">@carol@other.test</a> ` +
		`<a href="https://example.test/@me" class="u-url mention">@<span>me</span></a> ` +
		`<a href="https://example.test/@me" class="u-url mention">@me</a> ` +
		`<a href="https://remote.test/tags/x" class="mention hashtag">#<span>x</span></a> hi</p>`
	out := string(app.renderHTML(content, nil, nil))
	for _, want := range []string{`href="/@alice@remote.test"`, `href="/@carol@other.test"`, `href="/@me"`, `href="/tags/x"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered HTML lacks %s: %s", want, out)
		}
	}
	if strings.Contains(out, `href="https://remote.test/@alice"`) {
		t.Errorf("mention still points to the remote instance: %s", out)
	}
	author := &Account{ID: "a1", Username: "bob", Domain: "remote.test", URI: "https://remote.test/users/bob"}
	st := &Status{Account: author, Content: content, Visibility: "public"}
	c := app.replyCompose(me, st)
	if c.Text != "@bob@remote.test @alice@remote.test @carol@other.test " {
		t.Errorf("reply prefill = %q", c.Text)
	}
}
