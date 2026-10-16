package server

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestFetchReplies(t *testing.T) {
	la, r, _, _ := featureFixture(t)
	note := func(id, inReplyTo, content, replies string) string {
		doc := `{"id": "{{R}}` + id + `", "type": "Note", "attributedTo": "{{R}}/users/alice", "content": "<p>` + content +
			`</p>", "to": ["https://www.w3.org/ns/activitystreams#Public"], "published": "2026-01-01T00:00:00Z"`
		if inReplyTo != "" {
			doc += `, "inReplyTo": "{{R}}` + inReplyTo + `"`
		}
		if replies != "" {
			doc += `, "replies": ` + replies
		}
		return doc + "}"
	}
	// The root lists its replies by IRI on a separate page; the first reply
	// embeds its own collection. /n/3 claims to be a reply but isn't.
	r.put("/n/1", note("/n/1", "", "root post", `"{{R}}/n/1/replies"`))
	r.put("/n/1/replies", `{"id": "{{R}}/n/1/replies", "type": "Collection", "first": "{{R}}/n/1/replies?page=1"}`)
	r.put("/n/1/replies?page=1", `{"id": "{{R}}/n/1/replies?page=1", "type": "CollectionPage", "items": ["{{R}}/n/2", "{{R}}/n/3"]}`)
	r.put("/n/2", note("/n/2", "/n/1", "first reply", `{"id": "{{R}}/n/2/replies", "type": "Collection", "first": {"type": "CollectionPage", "items": ["{{R}}/n/4"]}}`))
	r.put("/n/3", note("/n/3", "/n/99", "unrelated post smuggled in", ""))
	r.put("/n/4", note("/n/4", "/n/2", "nested reply", ""))

	ctx := context.Background()
	root, err := la.resolveStatus(ctx, r.Base+"/n/1")
	if err != nil {
		t.Fatal(err)
	}
	if n := la.fetchReplies(ctx, root.URI); n != 2 {
		t.Errorf("stored %d replies, want 2", n)
	}
	for _, id := range []string{"/n/2", "/n/4"} {
		if _, err := la.StatusByURI(r.Base + id); err != nil {
			t.Errorf("%s wasn't fetched", id)
		}
	}
	if _, err := la.StatusByURI(r.Base + "/n/3"); err == nil {
		t.Error("a post that doesn't reply to the root was stored")
	}
	if n := la.fetchReplies(ctx, root.URI); n != 0 {
		t.Errorf("second run stored %d", n)
	}

	// The web thread starts a fetch once per thread and says so.
	web := webClient(t, la, "bob", "password123")
	page := func() string {
		resp, err := web.Get(la.base + root.Path())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if p := page(); !strings.Contains(p, "Looking for more replies") || !strings.Contains(p, "nested reply") {
		t.Error("thread page doesn't show the replies or the fetch note")
	}
	if strings.Contains(page(), "Looking for more replies") {
		t.Error("second view fetched again")
	}
}
