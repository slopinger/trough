package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestWebInteractions(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	cat, _ := la.CreateLocalUser("cat", "password123", false)
	dan, _ := la.CreateLocalUser("dan", "password123", false)
	post, _ := la.PostStatus(context.Background(), bob, PostInput{Text: "hello pigs", Visibility: "public"})
	la.Like(cat, post)
	la.Like(dan, post)
	la.Boost(dan, post)
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
	thread := get(http.DefaultClient, post.Path())
	for _, want := range []string{`href="/s/` + post.ID + `/boosts"><b>1</b> boost<`, `href="/s/` + post.ID + `/favourites"><b>2</b> favourites<`} {
		if !strings.Contains(thread, want) {
			t.Errorf("thread lacks %s", want)
		}
	}
	favs := get(http.DefaultClient, "/s/"+post.ID+"/favourites")
	if !strings.Contains(favs, "Favourited by") || !strings.Contains(favs, `href="/@cat"`) || !strings.Contains(favs, `href="/@dan"`) {
		t.Error("favourites list incomplete")
	}
	if boosts := get(http.DefaultClient, "/s/"+post.ID+"/boosts"); !strings.Contains(boosts, `href="/@dan"`) || strings.Contains(boosts, `class="dn">cat<`) {
		t.Error("boosts list wrong")
	}
}
