package server

import (
	"context"
	"testing"
)

func inboxFixtures(t *testing.T) (*App, *Account, *Account, *Status) {
	t.Helper()
	app := testApp(t)
	local, err := app.CreateLocalUser("localtest", "test-password", false)
	if err != nil {
		t.Fatal(err)
	}
	remote := &Account{Username: "remote", Domain: "remote.test", URI: "https://remote.test/users/remote", Inbox: "https://remote.test/inbox", ActorType: "Person"}
	if err := app.saveAccount(remote); err != nil {
		t.Fatal(err)
	}
	status, err := app.PostStatus(context.Background(), local, PostInput{Text: "test post", Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	return app, local, remote, status
}

func TestUndoLikeRemovesNotification(t *testing.T) {
	for _, embedded := range []bool{false, true} {
		name := "uri"
		if embedded {
			name = "embedded"
		}
		t.Run(name, func(t *testing.T) {
			app, local, remote, status := inboxFixtures(t)
			like := obj{"id": remote.URI + "/likes/1", "type": "Like", "actor": remote.URI, "object": status.URI}
			if err := app.inLike(remote, like); err != nil {
				t.Fatal(err)
			}
			if got := app.unreadNotifications(local.ID); got != 1 {
				t.Fatalf("initial notifications=%d", got)
			}
			var object any = like["id"]
			if embedded {
				object = like
			}
			undo := obj{"type": "Undo", "actor": remote.URI, "object": object}
			if err := app.inUndo(remote, undo); err != nil {
				t.Fatal(err)
			}
			if got := app.countInt(`SELECT COUNT(*) FROM likes`); got != 0 {
				t.Fatalf("likes=%d", got)
			}
			if got := app.unreadNotifications(local.ID); got != 0 {
				t.Fatalf("stale notifications=%d", got)
			}
			// Repeated deliveries are harmless.
			if err := app.inUndo(remote, undo); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUndoFollowRemovesNotifications(t *testing.T) {
	for _, locked := range []bool{false, true} {
		name := "accepted"
		if locked {
			name = "pending"
		}
		t.Run(name, func(t *testing.T) {
			app, local, remote, _ := inboxFixtures(t)
			local.Locked = locked
			if err := app.saveAccount(local); err != nil {
				t.Fatal(err)
			}
			follow := obj{"id": remote.URI + "/follows/1", "type": "Follow", "actor": remote.URI, "object": local.URI}
			if err := app.inFollow(remote, follow); err != nil {
				t.Fatal(err)
			}
			if got := app.unreadNotifications(local.ID); got != 1 {
				t.Fatalf("initial notifications=%d", got)
			}
			if err := app.inUndo(remote, obj{"type": "Undo", "actor": remote.URI, "object": follow["id"]}); err != nil {
				t.Fatal(err)
			}
			if got := app.followRequestCount(local.ID); got != 0 {
				t.Fatalf("pending requests=%d", got)
			}
			if got := app.unreadNotifications(local.ID); got != 0 {
				t.Fatalf("stale notifications=%d", got)
			}
		})
	}
}

func TestUndoCannotDeleteAnotherActorsActivity(t *testing.T) {
	app, local, remote, status := inboxFixtures(t)
	like := obj{"id": remote.URI + "/likes/1", "type": "Like", "actor": remote.URI, "object": status.URI}
	if err := app.inLike(remote, like); err != nil {
		t.Fatal(err)
	}
	if err := app.inUndo(local, obj{"type": "Undo", "actor": local.URI, "object": like}); err == nil {
		t.Fatal("accepted an embedded Undo for another actor")
	}
	if err := app.inUndo(local, obj{"type": "Undo", "actor": local.URI, "object": like["id"]}); err != nil {
		t.Fatal(err)
	}
	if got := app.countInt(`SELECT COUNT(*) FROM likes`); got != 1 {
		t.Fatalf("someone else's like removed: %d", got)
	}
}
