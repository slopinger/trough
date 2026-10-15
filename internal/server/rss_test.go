package server

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRSSFeeds(t *testing.T) {
	la := startLiveApp(t)
	ann, _ := la.CreateLocalUser("ann", "password123", false)
	ctx := context.Background()
	first, _ := la.PostStatus(ctx, ann, PostInput{Text: "hello #feeds", Visibility: "public"})
	la.PostStatus(ctx, ann, PostInput{Text: "followers only #feeds", Visibility: "private"})
	la.PostStatus(ctx, ann, PostInput{Text: "a reply", Visibility: "public", InReplyTo: first.ID})

	get := func(path string) (string, string) {
		resp, err := http.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		return string(b), resp.Header.Get("Content-Type")
	}
	for _, path := range []string{"/@ann.rss", "/tags/feeds.rss"} {
		body, ct := get(path)
		var feed rssFeed
		if err := xml.Unmarshal([]byte(body), &feed); err != nil || !strings.HasPrefix(ct, "application/rss+xml") {
			t.Fatalf("%s: %v %s", path, err, ct)
		}
		if len(feed.Channel.Items) != 1 || feed.Channel.Items[0].GUID != first.URI || !strings.Contains(feed.Channel.Items[0].Description, "hello") {
			t.Fatalf("%s items = %+v", path, feed.Channel.Items)
		}
	}
	if page, _ := get("/@ann"); !strings.Contains(page, `href="/@ann.rss"`) {
		t.Fatal("profile doesn't link its feed")
	}
}
