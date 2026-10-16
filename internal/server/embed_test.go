package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestEmbed(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	ctx := context.Background()
	pub, _ := la.PostStatus(ctx, bob, PostInput{Text: "embed me <3", Visibility: "public"})
	secret, _ := la.PostStatus(ctx, bob, PostInput{Text: "followers only", Visibility: "private"})
	get := func(c *http.Client, path string) (*http.Response, string) {
		t.Helper()
		resp, err := c.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	// The embed page may be framed anywhere and shows just the post.
	resp, body := get(http.DefaultClient, pub.Path()+"/embed")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors *") ||
		resp.Header.Get("X-Frame-Options") != "" {
		t.Fatalf("embed: %d, headers %v", resp.StatusCode, resp.Header)
	}
	if !strings.Contains(body, "embed me &lt;3") || !strings.Contains(body, `class="embed"`) || strings.Contains(body, "sidebar") {
		t.Error("embed page content wrong")
	}
	if resp, _ := get(http.DefaultClient, secret.Path()+"/embed"); resp.StatusCode != 404 {
		t.Errorf("followers-only post embeddable: %d", resp.StatusCode)
	}
	// other pages still refuse framing
	if resp, _ := get(http.DefaultClient, pub.Path()); !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("the post page may be framed")
	}

	// oEmbed, discovered from the post page
	if _, page := get(http.DefaultClient, pub.Path()); !strings.Contains(page, `type="application/json+oembed"`) {
		t.Error("no oEmbed discovery link")
	}
	api := &apiClient{t: t, base: la.base}
	for _, u := range []string{pub.URL, pub.URI, la.base + pub.Path()} {
		m := api.ok("GET", "/api/oembed?"+url.Values{"url": {u}}.Encode(), nil).(map[string]any)
		if m["type"] != "rich" || !strings.Contains(m["html"].(string), pub.Path()+`/embed" class="trough-embed"`) {
			t.Errorf("oembed for %s: %v", u, m)
		}
	}
	for _, u := range []string{secret.URL, "https://elsewhere.example/@bob/" + pub.ID, la.base + "/s/nope"} {
		if code, _, _ := api.do("GET", "/api/oembed?"+url.Values{"url": {u}}.Encode(), nil); code != 404 {
			t.Errorf("oembed for %s: %d", u, code)
		}
	}
	if resp, js := get(http.DefaultClient, "/embed.js"); resp.StatusCode != 200 || !strings.Contains(js, "trough:height") {
		t.Error("embed.js missing")
	}

	// The post menu leads to the code.
	web := webClient(t, la, "bob", "password123")
	if _, page := get(web, pub.Path()); !strings.Contains(page, pub.Path()+"/embed_code") {
		t.Error("no Embed menu item")
	}
	if _, page := get(web, pub.Path()+"/embed_code"); !strings.Contains(page, "&lt;iframe src=") || !strings.Contains(page, "/embed.js") {
		t.Error("embed code page lacks the code")
	}
}
