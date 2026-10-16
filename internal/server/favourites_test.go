package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestWebFavourites(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	cat, _ := la.CreateLocalUser("cat", "password123", false)
	post, _ := la.PostStatus(context.Background(), bob, PostInput{Text: "hello pigs", Visibility: "public"})
	la.Like(cat, post)
	get := func(c *http.Client, path string) string {
		t.Helper()
		resp, err := c.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if page := get(webClient(t, la, "cat", "password123"), "/favourites"); !strings.Contains(page, "hello pigs") || !strings.Contains(page, `href="/favourites"`) {
		t.Error("cat's favourites page lacks the post or the sidebar link")
	}
	if page := get(webClient(t, la, "bob", "password123"), "/favourites"); strings.Contains(page, "hello pigs") {
		t.Error("bob sees a post he didn't favourite")
	}
}
