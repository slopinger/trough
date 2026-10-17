package server

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"testing"
)

func TestForceSensitive(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	boss, _ := la.CreateLocalUser("boss", "password123", true)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	spam, _ := la.CreateLocalUser("spam", "password123", false)
	var pic bytes.Buffer
	png.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	withMedia := func() *Status {
		at, err := la.saveImage(spam, pic.Bytes(), "")
		if err != nil {
			t.Fatal(err)
		}
		st, err := la.PostStatus(ctx, spam, PostInput{Text: "look", Visibility: "public", MediaIDs: []string{at.ID}})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	before := withMedia()

	reportID, _ := la.FileReport(bob, spam, []string{before.ID}, "gore", false)
	if _, err := la.resolveReport(boss, reportID, "sensitive", "", "Please mark these yourself."); err != nil {
		t.Fatal(err)
	}
	spam, _ = la.AccountByID(spam.ID)
	if !spam.ForceSensitive {
		t.Fatal("not marked")
	}
	if st, _ := la.StatusByID(before.ID); !st.Sensitive {
		t.Error("an earlier post isn't shown as sensitive")
	}
	after := withMedia()
	if la.countInt(`SELECT sensitive FROM statuses WHERE id=?`, after.ID) != 1 {
		t.Error("a new post with media wasn't stored as sensitive")
	}
	if text, _ := la.PostStatus(ctx, spam, PostInput{Text: "just words", Visibility: "public"}); la.countInt(`SELECT sensitive FROM statuses WHERE id=?`, text.ID) != 0 {
		t.Error("a post without media was marked sensitive")
	}
	ws := la.loadWarnings(`w.account_id=?`, spam.ID)
	if len(ws) != 1 || ws[0].Action != "sensitive" || ws[0].Summary() != "Your media is marked as sensitive" {
		t.Fatalf("warning: %+v", ws)
	}

	// an approved appeal lifts it; earlier posts show as they were stored
	la.appeal(spam, ws[0].ID, "it's just a sunset")
	if _, err := la.decideAppeal(boss, ws[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if spam, _ = la.AccountByID(spam.ID); spam.ForceSensitive {
		t.Error("approving the appeal didn't lift it")
	}
	if st, _ := la.StatusByID(before.ID); st.Sensitive {
		t.Error("the earlier post is still sensitive")
	}

	// remote accounts too, from the admin page
	r := newRemote(t, la)
	r.put("/users/alice", mastodonActor)
	alice, _ := la.getAccount(ctx, r.Base+"/users/alice")
	la.db.Exec(`INSERT INTO statuses (id,uri,account_id,visibility,created_at) VALUES ('rs1',?,?,'public',0)`, r.Base+"/s/1", alice.ID)
	if _, err := la.moderateAccount(boss, alice, "sensitive", ""); err != nil {
		t.Fatal(err)
	}
	if st, _ := la.StatusByID("rs1"); !st.Sensitive {
		t.Error("the remote post isn't shown as sensitive")
	}
	if alice, _ = la.fetchAccount(ctx, alice.URI); !alice.ForceSensitive {
		t.Error("refetching the actor lifted it")
	}
}
