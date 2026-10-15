package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
)

func TestArchive(t *testing.T) {
	la := startLiveApp(t)
	ann, _ := la.CreateLocalUser("ann", "password123", false)
	ctx := context.Background()
	pub, _ := la.PostStatus(ctx, ann, PostInput{Text: "public", Visibility: "public"})
	la.PostStatus(ctx, ann, PostInput{Text: "just for followers", Visibility: "private"})
	la.Bookmark(ann, pub)
	la.Like(ann, pub)

	c := webClient(t, la, "ann", "password123")
	resp, err := c.Get(la.base + "/settings/export/archive.zip")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a zip (%d, %s): %v", resp.StatusCode, resp.Header.Get("Content-Type"), err)
	}
	files := map[string]map[string]any{}
	for _, f := range zr.File {
		r, _ := f.Open()
		var m map[string]any
		json.NewDecoder(r).Decode(&m)
		r.Close()
		files[f.Name] = m
	}
	if files["actor.json"]["preferredUsername"] != "ann" {
		t.Fatalf("actor.json = %v", files["actor.json"])
	}
	if n := files["outbox.json"]["totalItems"]; n != float64(2) {
		t.Fatalf("outbox has %v items, want both posts", n)
	}
	for _, f := range []string{"likes.json", "bookmarks.json"} {
		if items := files[f]["orderedItems"].([]any); len(items) != 1 || items[0] != pub.URI {
			t.Fatalf("%s = %v", f, items)
		}
	}
	// once an hour
	resp, _ = c.Get(la.base + "/settings/export/archive.zip")
	resp.Body.Close()
	if resp.Header.Get("Content-Type") == "application/zip" {
		t.Fatal("second archive within the hour")
	}
}
