package server

import (
	"context"
	"strings"
	"testing"
)

// Mastodon forwards replies (and their deletion) to the followers of the
// person replied to, signed by the forwarding server. trough fetches the
// post from its origin instead of trusting the forwarded copy.
func TestForwardedActivities(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	ctx := context.Background()
	mine, _ := la.PostStatus(ctx, bob, PostInput{Text: "what do pigs dream of?", Visibility: "public"})

	forwarder, origin := newRemote(t, la), newRemote(t, la)
	forwarder.put("/users/alice", mastodonActor)
	origin.put("/users/alice", mastodonActor)
	carolURI := origin.Base + "/users/alice"
	note := func(id, content, inReplyTo string) string {
		return `{"id": "{{R}}/notes/` + id + `", "type": "Note", "attributedTo": "{{R}}/users/alice", "content": "<p>` + content + `</p>",
			"inReplyTo": ` + inReplyTo + `, "to": ["https://www.w3.org/ns/activitystreams#Public"], "published": "2026-01-01T00:00:00Z"}`
	}
	origin.put("/notes/1", note("1", "mud, mostly", `"`+mine.URI+`"`))
	origin.put("/notes/2", note("2", "unrelated chatter", "null"))
	forwardKey := forwarder.Base + "/users/alice#main-key"
	forward := func(typ, id string, object any) int {
		return forwarder.send(obj{"@context": asContext, "id": origin.Base + "/activities/" + id, "type": typ, "actor": carolURI, "object": object}, forwardKey, sigCavage)
	}

	// The forwarded copy claims other content; the origin's is stored.
	forged := obj{"id": origin.Base + "/notes/1", "type": "Note", "attributedTo": carolURI, "content": "<p>forged</p>", "inReplyTo": mine.URI,
		"to": []any{"https://www.w3.org/ns/activitystreams#Public"}}
	if code := forward("Create", "1", forged); code != 202 {
		t.Fatalf("forwarded Create: %d", code)
	}
	waitFor(t, "the reply to be fetched", func() bool { _, err := la.StatusByURI(origin.Base + "/notes/1"); return err == nil })
	st, _ := la.StatusByURI(origin.Base + "/notes/1")
	if !strings.Contains(st.Content, "mud, mostly") || st.InReplyToID != mine.ID {
		t.Errorf("stored %q replying to %q", st.Content, st.InReplyToID)
	}

	// Posts that don't concern anyone here aren't stored (called directly:
	// through the inbox there is nothing to wait for).
	act := func(typ string, object any) obj { return obj{"type": typ, "actor": carolURI, "object": object} }
	if err := la.inForwarded(ctx, act("Create", origin.Base+"/notes/2")); err != nil {
		t.Fatal(err)
	}
	if _, err := la.StatusByURI(origin.Base + "/notes/2"); err == nil {
		t.Error("an unrelated forwarded post was stored")
	}

	// A forwarded Delete only counts once the origin says the post is gone.
	la.inForwarded(ctx, act("Delete", origin.Base+"/notes/1"))
	if _, err := la.StatusByURI(origin.Base + "/notes/1"); err != nil {
		t.Fatal("deleted although the origin still has it")
	}
	origin.mu.Lock()
	delete(origin.docs, "/notes/1")
	origin.mu.Unlock()
	forward("Delete", "2", obj{"id": origin.Base + "/notes/1", "type": "Tombstone"})
	waitFor(t, "the reply to be deleted", func() bool { _, err := la.StatusByURI(origin.Base + "/notes/1"); return err != nil })

	// Other forwarded activities are still refused.
	if code := forward("Like", "3", mine.URI); code != 401 {
		t.Errorf("forwarded Like: %d", code)
	}
}
