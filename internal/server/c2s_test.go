package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.freifick.net/slop/trough/internal/ap"
	"git.freifick.net/slop/trough/internal/ulid"
)

func TestC2S(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	la.CreateLocalUser("eve", "password123", false)
	token := oauthLogin(t, la, "bob", "password123", "read write")
	post := func(tok string, act obj) (int, string) {
		t.Helper()
		b, _ := json.Marshal(act)
		req, _ := http.NewRequest("POST", bob.Outbox, bytes.NewReader(b))
		req.Header.Set("Content-Type", ap.LDMimeType)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Location")
	}
	ok := func(act obj) string {
		t.Helper()
		code, loc := post(token, act)
		if code != 201 || loc == "" {
			t.Fatalf("%s: %d (Location %q)", ap.Type(act), code, loc)
		}
		return loc
	}
	note := obj{"type": "Note", "content": "hi"}

	// only the owner, with a write token
	if code, _ := post("", note); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := post(oauthLogin(t, la, "eve", "password123", "read write"), note); code != 403 {
		t.Errorf("someone else's token: %d", code)
	}
	if code, _ := post(oauthLogin(t, la, "bob", "password123", "read"), note); code != 403 {
		t.Errorf("read-only token: %d", code)
	}

	// Create, with source text and public addressing on the activity
	loc := ok(obj{"@context": asContext, "type": "Create", "to": []any{ap.Public}, "cc": []any{bob.FollowersURI},
		"object": obj{"type": "Note", "content": "<p>ignored</p>", "source": obj{"content": "hello from a C2S client #c2s", "mediaType": "text/plain"}}})
	st, err := la.StatusByURI(strings.TrimSuffix(loc, "/activity"))
	if err != nil || st.Visibility != "public" || st.Source != "hello from a C2S client #c2s" {
		t.Fatalf("created %v %+v", err, st)
	}
	eventually(t, "Create federated", func() bool { return len(r.receivedOf("Create")) == 1 })

	// a bare Note addressed to alice only is a DM that mentions her
	loc = ok(obj{"type": "Note", "content": "psst", "to": []any{alice.URI}})
	dm, _ := la.StatusByURI(strings.TrimSuffix(loc, "/activity"))
	if dm.Visibility != "direct" || !strings.Contains(dm.Source, "@"+alice.Acct()) {
		t.Fatalf("DM = %+v", dm)
	}
	eventually(t, "DM delivered", func() bool { return len(r.receivedOf("Create")) == 2 })

	// replies, likes, boosts and their undos on a remote post
	theirs := &Status{ID: ulid.New(), URI: r.Base + "/users/alice/statuses/9", AccountID: alice.ID, Visibility: "public", Content: "<p>remote</p>", CreatedAt: time.Now()}
	la.insertStatus(theirs)
	loc = ok(obj{"type": "Note", "content": "a reply", "inReplyTo": theirs.URI, "to": []any{ap.Public}})
	reply, _ := la.StatusByURI(strings.TrimSuffix(loc, "/activity"))
	if reply.InReplyToID != theirs.ID {
		t.Errorf("reply not threaded: %+v", reply)
	}
	like := ok(obj{"type": "Like", "object": theirs.URI})
	eventually(t, "Like delivered", func() bool { return len(r.receivedOf("Like")) == 1 })
	if ap.ID(r.receivedOf("Like")[0].Activity["id"]) != like {
		t.Errorf("Location %s is not the Like's id", like)
	}
	ok(obj{"type": "Undo", "object": like})
	ok(obj{"type": "Announce", "object": theirs.URI})
	eventually(t, "Announce delivered", func() bool { return len(r.receivedOf("Announce")) == 1 })
	ok(obj{"type": "Undo", "object": obj{"type": "Announce", "object": theirs.URI}})
	eventually(t, "both Undos delivered", func() bool { return len(r.receivedOf("Undo")) == 2 })
	if la.countInt(`SELECT COUNT(*) FROM likes`)+la.countInt(`SELECT COUNT(*) FROM statuses WHERE boost_of_id != ''`) != 0 {
		t.Error("undo left a like or boost behind")
	}

	// edit and delete our own post, but not someone else's
	ok(obj{"type": "Update", "object": obj{"id": st.URI, "type": "Note", "content": "edited via C2S"}})
	if s, _ := la.StatusByID(st.ID); !strings.Contains(s.Content, "edited via C2S") {
		t.Errorf("not edited: %s", s.Content)
	}
	eventually(t, "Update delivered", func() bool { return len(r.receivedOf("Update")) == 1 })
	if code, _ := post(token, obj{"type": "Delete", "object": theirs.URI}); code != 404 {
		t.Errorf("deleted someone else's post: %d", code)
	}
	ok(obj{"type": "Delete", "object": st.URI})
	eventually(t, "Delete delivered", func() bool { return len(r.receivedOf("Delete")) == 1 })

	// follows
	r.put("/users/carol", strings.NewReplacer("users/alice", "users/carol", `"preferredUsername": "alice"`, `"preferredUsername": "carol"`).Replace(mastodonActor))
	follow := ok(obj{"type": "Follow", "object": r.Base + "/users/carol"})
	eventually(t, "Follow delivered", func() bool { return len(r.receivedOf("Follow")) == 1 })
	ok(obj{"type": "Undo", "object": follow})
	eventually(t, "Undo Follow delivered", func() bool { return len(r.receivedOf("Undo")) == 3 })

	if code, _ := post(token, obj{"type": "Move", "object": bob.URI}); code != 422 {
		t.Errorf("unsupported activity: %d", code)
	}

	// reading the inbox
	get := func(u, tok string) (int, obj) {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Accept", ap.MimeType)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m obj
		json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}
	if code, _ := get(bob.Inbox, ""); code != 401 {
		t.Errorf("anonymous inbox read: %d", code)
	}
	_, coll := get(bob.Inbox, token)
	_, page := get(ap.Str(coll, "first"), token)
	found := false
	for _, it := range ap.Objects(page["orderedItems"]) {
		if ap.ID(ap.Object(it["object"])["id"]) == theirs.URI {
			found = true
		}
	}
	if !found {
		t.Errorf("alice's post not in bob's inbox: %v", page)
	}
	_, actor := get(bob.URI, "")
	if ap.Str(ap.Object(actor["endpoints"]), "oauthTokenEndpoint") != la.base+"/oauth/token" {
		t.Errorf("actor endpoints = %v", actor["endpoints"])
	}
}

func TestC2SUploadMedia(t *testing.T) {
	la, r, bob, _ := featureFixture(t)
	token := oauthLogin(t, la, "bob", "password123", "read write")
	var img bytes.Buffer
	png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 4, 3)))
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "x.png")
	fw.Write(img.Bytes())
	mw.WriteField("object", `{"type": "Image", "name": "a tiny square"}`)
	mw.Close()
	_, actor := func() (int, obj) {
		req, _ := http.NewRequest("GET", bob.URI, nil)
		req.Header.Set("Accept", ap.MimeType)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m obj
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}()
	endpoint := ap.Str(ap.Object(actor["endpoints"]), "uploadMedia")
	req, _ := http.NewRequest("POST", endpoint, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var media obj
	json.NewDecoder(resp.Body).Decode(&media)
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != 201 || loc == "" || ap.Type(media) != "Image" || ap.Str(media, "name") != "a tiny square" {
		t.Fatalf("upload: %d %q %v", resp.StatusCode, loc, media)
	}
	post := func(act obj) int {
		b, _ := json.Marshal(act)
		req, _ := http.NewRequest("POST", bob.Outbox, bytes.NewReader(b))
		req.Header.Set("Content-Type", ap.MimeType)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(obj{"type": "Note", "content": "stolen", "to": []any{ap.Public}, "attachment": []any{obj{"type": "Image", "url": "https://elsewhere.example/x.png"}}}); code != 400 {
		t.Errorf("foreign attachment: %d", code)
	}
	if code := post(obj{"type": "Note", "content": "look", "to": []any{ap.Public}, "attachment": []any{obj{"type": "Image", "url": loc}}}); code != 201 {
		t.Fatalf("post with attachment: %d", code)
	}
	eventually(t, "Create delivered", func() bool { return len(r.receivedOf("Create")) == 1 })
	atts := ap.Objects(ap.Object(r.receivedOf("Create")[0].Activity["object"])["attachment"])
	if len(atts) != 1 || ap.Str(atts[0], "name") != "a tiny square" {
		t.Fatalf("federated attachment = %v", atts)
	}
}
