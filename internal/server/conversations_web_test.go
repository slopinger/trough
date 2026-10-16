package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestWebConversations(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	cat, _ := la.CreateLocalUser("cat", "password123", false)
	ctx := context.Background()
	dm, _ := la.PostStatus(ctx, bob, PostInput{Text: "@cat secret pig plans", Visibility: "direct"})
	la.PostStatus(ctx, cat, PostInput{Text: "@bob count me in", Visibility: "direct", InReplyTo: dm.ID})
	la.PostStatus(ctx, cat, PostInput{Text: "public oink", Visibility: "public"})

	web := webClient(t, la, "bob", "password123")
	get := func(path string) (string, string) {
		t.Helper()
		resp, err := web.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.Request.URL.Path
	}
	page, _ := get("/conversations")
	if !strings.Contains(page, `class="conv unread"`) || !strings.Contains(page, "count me in") || strings.Contains(page, "public oink") ||
		!strings.Contains(page, `value="direct" selected`) || !strings.Contains(page, `href="/conversations"`) {
		t.Fatal("conversation list wrong")
	}
	// opening marks it read and lands on the thread
	if body, path := get("/conversations/" + dm.ID); !strings.HasPrefix(path, "/s/") || !strings.Contains(body, "secret pig plans") {
		t.Errorf("open went to %s", path)
	}
	if page, _ = get("/conversations"); strings.Contains(page, `class="conv unread"`) {
		t.Error("still unread after opening")
	}
	csrf := strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0]
	req, _ := http.NewRequest("POST", la.base+"/conversations/"+dm.ID+"/hide", strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", la.base)
	if resp, err := web.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	if page, _ = get("/conversations"); strings.Contains(page, "count me in") {
		t.Error("hidden conversation still listed")
	}
	if _, path := get("/conversations/nope"); path != "/conversations/nope" {
		t.Error("unknown conversation redirected")
	}
}
