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
	badge := func() bool { return strings.Contains(page, `dm-badge">1</b>`) }
	if !badge() {
		t.Error("sidebar has no unread badge")
	}
	// opening marks it read and lands on the thread
	if body, path := get("/conversations/" + dm.ID); !strings.HasPrefix(path, "/s/") || !strings.Contains(body, "secret pig plans") {
		t.Errorf("open went to %s", path)
	}
	if page, _ = get("/conversations"); strings.Contains(page, `class="conv unread"`) {
		t.Error("still unread after opening")
	}
	if badge() || !strings.Contains(page, `dm-badge" hidden>0</b>`) {
		t.Error("badge still shown after opening")
	}
	// a new message counts again, and the live poll reports it
	la.PostStatus(ctx, cat, PostInput{Text: "@bob bring snacks", Visibility: "direct", InReplyTo: dm.ID})
	if poll, _ := get("/timeline/new"); !strings.Contains(poll, `"dms":1`) {
		t.Errorf("poll = %s", poll)
	}
	if page, _ = get("/conversations"); !badge() {
		t.Error("new message didn't bring the badge back")
	}
	get("/conversations/" + dm.ID)
	page, _ = get("/conversations")
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

func TestFilteredConversations(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	dan, _ := la.CreateLocalUser("dan", "password123", false)
	eve, _ := la.CreateLocalUser("eve", "password123", false)
	ctx := context.Background()
	// strangers' private mentions are filtered by default
	la.PostStatus(ctx, dan, PostInput{Text: "@bob buy my oinkcoin", Visibility: "direct"})
	la.PostStatus(ctx, eve, PostInput{Text: "@bob hi, we met at the trough meetup", Visibility: "direct"})
	web := webClient(t, la, "bob", "password123")
	page := func() string {
		t.Helper()
		resp, err := web.Get(la.base + "/conversations")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	p := page()
	if strings.Contains(p, "oinkcoin") || strings.Contains(p, "meetup") || !strings.Contains(p, "2 conversations from people you may not know wait") {
		t.Fatal("filtered DMs are listed")
	}
	if la.unreadConversations(bob) != 0 {
		t.Error("badge counts filtered DMs")
	}
	la.acceptNotificationRequest(bob, eve.ID)
	if la.unreadConversations(bob) != 1 {
		t.Error("accepted DM isn't counted right away")
	}
	la.dismissNotificationRequest(bob, dan.ID)
	if p = page(); !strings.Contains(p, "meetup") || strings.Contains(p, "oinkcoin") || strings.Contains(p, "may not know") {
		t.Error("accepting should show eve's DM, dismissing should hide dan's")
	}
	if la.unreadConversations(bob) != 1 {
		t.Error("accepted DM isn't counted")
	}
}
