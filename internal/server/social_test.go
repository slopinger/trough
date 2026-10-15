package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPollsFederate(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	ctx := context.Background()
	poll, err := newPoll([]string{"tea", "coffee", "", ""}, "1d", false)
	if err != nil {
		t.Fatal(err)
	}
	st, err := la.PostStatus(ctx, bob, PostInput{Text: "tea or coffee?", Visibility: "public", Poll: poll})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "Question delivered", func() bool { return len(r.receivedOf("Create")) == 1 })
	q := objOf(r.receivedOf("Create")[0].Activity["object"])
	opts := objsOf(q["oneOf"])
	if typeOf(q) != "Question" || len(opts) != 2 || str(opts[1], "name") != "coffee" || str(q, "endTime") == "" {
		t.Fatalf("question = %v", q)
	}
	// alice votes coffee the way Mastodon does
	vote := obj{"id": r.Base + "/users/alice#votes/1", "type": "Note", "name": "coffee", "attributedTo": alice.URI,
		"inReplyTo": st.URI, "to": []any{bob.URI}}
	r.send(obj{"@context": asContext, "id": r.Base + "/users/alice#votes/1/activity", "type": "Create", "actor": alice.URI, "to": []any{bob.URI}, "object": vote},
		r.Base+"/users/alice#main-key", sigCavage)
	eventually(t, "vote counted", func() bool {
		s, _ := la.StatusByID(st.ID)
		return s.Poll.Options[1].Votes == 1
	})
	if _, err := la.StatusByURI(r.Base + "/users/alice#votes/1"); err == nil {
		t.Error("vote stored as a post")
	}
	eventually(t, "Update with new counts", func() bool {
		for _, u := range r.receivedOf("Update") {
			o := objOf(u.Activity["object"])
			if typeOf(o) == "Question" && objsOf(o["oneOf"])[1]["replies"].(obj)["totalItems"].(float64) == 1 {
				return true
			}
		}
		return false
	})
	if la.Vote(bob, st, []int{0}) == nil {
		t.Error("voted in own poll")
	}
	// bob votes in alice's poll
	qid := r.Base + "/users/alice/statuses/poll"
	question := `{"id": "` + qid + `", "type": "Question", "attributedTo": "{{R}}/users/alice", "content": "<p>cats or dogs?</p>",
		"to": ["https://www.w3.org/ns/activitystreams#Public"], "endTime": "` + apTime(time.Now().Add(time.Hour)) + `",
		"anyOf": [{"type": "Note", "name": "cats", "replies": {"type": "Collection", "totalItems": 3}}, {"type": "Note", "name": "dogs", "replies": {"type": "Collection", "totalItems": 1}}]}`
	r.put("/users/alice/statuses/poll", question)
	r.send(obj{"@context": asContext, "id": qid + "/activity", "type": "Create", "actor": alice.URI, "to": []any{asPublic}, "object": mustJSON(t, r.expand(question))},
		r.Base+"/users/alice#main-key", sigCavage)
	var rq *Status
	eventually(t, "remote poll stored", func() bool { rq, err = la.StatusByURI(qid); return err == nil && rq.Poll != nil })
	full, _ := la.loadStatus(bob, rq.ID)
	if !full.Poll.Multiple || full.Poll.Options[0].Votes != 3 {
		t.Fatalf("poll = %+v", full.Poll)
	}
	if err := la.Vote(bob, full, []int{0, 1}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "votes delivered", func() bool {
		n := 0
		for _, c := range r.receivedOf("Create") {
			if o := objOf(c.Activity["object"]); str(o, "inReplyTo") == qid && str(o, "name") != "" {
				n++
			}
		}
		return n == 2
	})
	full, _ = la.loadStatus(bob, rq.ID)
	if !full.MyVotes[0] || !full.MyVotes[1] || full.Poll.Options[0].Votes != 4 {
		t.Fatalf("after voting: %+v %+v", full.MyVotes, full.Poll)
	}
	if la.Vote(bob, full, []int{0}) == nil {
		t.Error("voted twice")
	}
}

func TestReactionsAndQuotes(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	ctx := context.Background()
	mine, _ := la.PostStatus(ctx, bob, PostInput{Text: "react to me", Visibility: "public"})
	key := r.Base + "/users/alice#main-key"
	// Misskey-style reaction
	r.send(obj{"@context": asContext, "id": r.Base + "/likes/1", "type": "Like", "actor": alice.URI, "object": mine.URI, "content": "🍮", "_misskey_reaction": "🍮"}, key, sigCavage)
	// Pleroma-style EmojiReact with a custom emoji
	r.send(obj{"@context": asContext, "id": r.Base + "/react/1", "type": "EmojiReact", "actor": alice.URI, "object": mine.URI, "content": ":blobcat:",
		"tag": []any{obj{"type": "Emoji", "name": ":blobcat:", "icon": obj{"type": "Image", "url": r.Base + "/blobcat.png"}}}}, key, sigCavage)
	eventually(t, "reactions stored", func() bool { return la.countInt(`SELECT COUNT(*) FROM reactions WHERE status_id=?`, mine.ID) == 2 })
	full, _ := la.loadStatus(bob, mine.ID)
	if len(full.Reactions) != 2 || full.Likes != 1 {
		t.Fatalf("reactions = %+v likes=%d", full.Reactions, full.Likes)
	}
	r.send(obj{"@context": asContext, "id": r.Base + "/undo/1", "type": "Undo", "actor": alice.URI,
		"object": obj{"id": r.Base + "/react/1", "type": "EmojiReact", "actor": alice.URI, "object": mine.URI}}, key, sigCavage)
	eventually(t, "reaction undone", func() bool { return la.countInt(`SELECT COUNT(*) FROM reactions WHERE status_id=?`, mine.ID) == 1 })

	// bob reacts to alice's post
	theirs := &Status{ID: newID(), URI: r.Base + "/s/1", AccountID: alice.ID, Visibility: "public", Content: "<p>hi</p>", CreatedAt: time.Now()}
	la.insertStatus(theirs)
	tf, _ := la.loadStatus(bob, theirs.ID)
	if err := la.React(bob, tf, "🤓"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "EmojiReact delivered", func() bool { return len(r.receivedOf("EmojiReact")) == 1 })
	if er := r.receivedOf("EmojiReact")[0].Activity; str(er, "content") != "🤓" || idOf(er["object"]) != theirs.URI {
		t.Fatalf("EmojiReact = %v", er)
	}

	// they quote bob (Misskey style, without approval; FEP-044f quotes are in TestQuoteHandshake)
	for i, field := range []string{"_misskey_quote"} {
		nid := r.Base + "/notes/q" + itoa(i)
		note := obj{"id": nid, "type": "Note", "attributedTo": alice.URI, "content": "<p>look at this<span class=\"quote-inline\"><br>RE: " + mine.URL + "</span></p>",
			"to": []any{asPublic}, "published": apTime(time.Now()), field: mine.URI}
		r.send(obj{"@context": asContext, "id": nid + "/activity", "type": "Create", "actor": alice.URI, "to": []any{asPublic}, "object": note}, key, sigCavage)
		var q *Status
		eventually(t, field+" quote stored", func() bool { q, _ = la.StatusByURI(nid); return q != nil })
		qf, _ := la.loadStatus(bob, q.ID)
		if qf.Quote == nil || qf.Quote.ID != mine.ID {
			t.Fatalf("%s: quote not hydrated: %+v", field, qf)
		}
	}
	if n := la.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=? AND type='quote'`, bob.ID); n != 1 {
		t.Errorf("quote notifications = %d", n)
	}
	// bob quotes alice
	qs, err := la.PostStatus(ctx, bob, PostInput{Text: "so true", Visibility: "public", QuoteOf: theirs.ID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "quote Create delivered", func() bool {
		for _, c := range r.receivedOf("Create") {
			if o := objOf(c.Activity["object"]); str(o, "id") == qs.URI {
				return str(o, "quote") == theirs.URI && str(o, "_misskey_quote") == theirs.URI && strings.Contains(str(o, "content"), "RE:")
			}
		}
		return false
	})
}

// TestQuoteHandshake covers FEP-044f quote approval in both directions, the
// way Mastodon 4.5+ does it.
func TestQuoteHandshake(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	ctx := context.Background()
	key := r.Base + "/users/alice#main-key"
	mine, _ := la.PostStatus(ctx, bob, PostInput{Text: "quote me", Visibility: "public"})
	if cq := objOf(objOf(la.noteJSON(mine)["interactionPolicy"])["canQuote"]); !reflect.DeepEqual(cq["automaticApproval"], []any{asPublic}) {
		t.Fatalf("canQuote = %v", cq)
	}

	// alice quotes bob: pending until her server asks and bob's approves
	nid := r.Base + "/notes/fq"
	note := obj{"id": nid, "type": "Note", "attributedTo": alice.URI, "content": "<p>look</p>", "to": []any{asPublic},
		"published": apTime(time.Now()), "quote": mine.URI}
	r.send(obj{"@context": asContext, "id": nid + "/activity", "type": "Create", "actor": alice.URI, "to": []any{asPublic}, "object": note}, key, sigCavage)
	var q *Status
	eventually(t, "quote stored", func() bool { q, _ = la.StatusByURI(nid); return q != nil })
	if qf, _ := la.loadStatus(bob, q.ID); qf.QuoteState != "pending" || qf.Quote != nil || la.apiStatus(bob, qf)["quote"].(obj)["state"] != "pending" {
		t.Fatalf("unapproved quote: state=%q quote=%v", qf.QuoteState, qf.Quote)
	}
	if n := la.countInt(`SELECT COUNT(*) FROM notifications WHERE type='quote'`); n != 0 {
		t.Fatalf("notified about a pending quote")
	}
	r.send(obj{"@context": asContext, "id": r.Base + "/quote_requests/1", "type": "QuoteRequest", "actor": alice.URI,
		"object": mine.URI, "instrument": note}, key, sigCavage)
	eventually(t, "Accept with a stamp", func() bool { return len(r.receivedOf("Accept")) == 1 })
	acc := r.receivedOf("Accept")[0].Activity
	stamp := idOf(acc["result"])
	if idOf(objOf(acc["object"])["id"]) != r.Base+"/quote_requests/1" || !strings.HasPrefix(stamp, bob.URI+"/quote_authorizations/") {
		t.Fatalf("Accept = %v", acc)
	}
	qf, _ := la.loadStatus(bob, q.ID)
	if qf.QuoteState != "" || qf.Quote == nil || qf.Quote.ID != mine.ID || qf.QuoteAuth != stamp {
		t.Fatalf("approved quote: state=%q auth=%q", qf.QuoteState, qf.QuoteAuth)
	}
	if n := la.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=? AND type='quote'`, bob.ID); n != 1 {
		t.Fatalf("quote notifications = %d", n)
	}
	code, auth := r.signedGet(stamp, "")
	if code != 200 || typeOf(auth) != "QuoteAuthorization" || str(auth, "attributedTo") != bob.URI ||
		idOf(auth["interactingObject"]) != nid || idOf(auth["interactionTarget"]) != mine.URI {
		t.Fatalf("stamp = %d %v", code, auth)
	}
	// her server then updates the note with the stamp; it stays approved
	note["quoteAuthorization"], note["content"] = stamp, "<p>look again</p>"
	r.send(obj{"@context": asContext, "id": nid + "#update", "type": "Update", "actor": alice.URI, "object": note}, key, sigCavage)
	eventually(t, "update applied", func() bool { qf, _ = la.loadStatus(bob, q.ID); return strings.Contains(qf.Content, "again") })
	if qf.QuoteState != "" || qf.Quote == nil {
		t.Fatalf("after update: state=%q", qf.QuoteState)
	}
	// bob revokes it
	if err := la.RevokeQuote(bob, mine, qf); err != nil {
		t.Fatal(err)
	}
	eventually(t, "stamp deleted", func() bool {
		for _, d := range r.receivedOf("Delete") {
			if idOf(d.Activity["object"]) == stamp {
				return true
			}
		}
		return false
	})
	if qf, _ = la.loadStatus(bob, q.ID); qf.QuoteState != "revoked" || qf.Quote != nil {
		t.Fatalf("after revoke: state=%q", qf.QuoteState)
	}
	if code, _ := r.signedGet(stamp, ""); code != 404 {
		t.Fatalf("revoked stamp served: %d", code)
	}

	// followers-only posts can't be quoted by others
	priv, _ := la.PostStatus(ctx, bob, PostInput{Text: "just friends", Visibility: "private"})
	r.send(obj{"@context": asContext, "id": r.Base + "/quote_requests/2", "type": "QuoteRequest", "actor": alice.URI,
		"object": priv.URI, "instrument": r.Base + "/notes/fq2"}, key, sigCavage)
	eventually(t, "Reject", func() bool { return len(r.receivedOf("Reject")) == 1 })
	if rej := r.receivedOf("Reject")[0].Activity; idOf(objOf(rej["object"])["id"]) != r.Base+"/quote_requests/2" {
		t.Fatalf("Reject = %v", rej)
	}

	// bob quotes a post of alice's server, which takes part in the handshake
	theirs := &Status{ID: newID(), URI: r.Base + "/s/q1", AccountID: alice.ID, Visibility: "public", Content: "<p>hi</p>", CreatedAt: time.Now(), QuotePolicy: "public"}
	la.insertStatus(theirs)
	bq, err := la.PostStatus(ctx, bob, PostInput{Text: "so true", Visibility: "public", QuoteOf: theirs.ID})
	if err != nil {
		t.Fatal(err)
	}
	if bq.QuoteState != "pending" {
		t.Fatalf("own quote state = %q", bq.QuoteState)
	}
	eventually(t, "QuoteRequest sent", func() bool { return len(r.receivedOf("QuoteRequest")) == 1 })
	qr := r.receivedOf("QuoteRequest")[0].Activity
	if idOf(qr["object"]) != theirs.URI || str(objOf(qr["instrument"]), "id") != bq.URI || str(objOf(qr["instrument"]), "quote") != theirs.URI {
		t.Fatalf("QuoteRequest = %v", qr)
	}
	r.send(obj{"@context": asContext, "id": r.Base + "/accepts/1", "type": "Accept", "actor": alice.URI,
		"object": obj{"id": str(qr, "id"), "type": "QuoteRequest", "actor": bob.URI, "object": theirs.URI, "instrument": bq.URI},
		"result": r.Base + "/users/alice/stamps/1"}, key, sigCavage)
	eventually(t, "Update with the stamp", func() bool {
		for _, u := range r.receivedOf("Update") {
			if o := objOf(u.Activity["object"]); str(o, "id") == bq.URI {
				return idOf(o["quoteAuthorization"]) == r.Base+"/users/alice/stamps/1"
			}
		}
		return false
	})
	if bf, _ := la.loadStatus(bob, bq.ID); bf.QuoteState != "" || bf.Quote == nil {
		t.Fatalf("accepted own quote: state=%q", bf.QuoteState)
	}
	// and alice later revokes it
	r.send(obj{"@context": asContext, "id": r.Base + "/stamps/1#delete", "type": "Delete", "actor": alice.URI,
		"object": r.Base + "/users/alice/stamps/1"}, key, sigCavage)
	eventually(t, "revoked", func() bool { bf, _ := la.loadStatus(bob, bq.ID); return bf.QuoteState == "revoked" })

	// a post nobody may quote
	closed := &Status{ID: newID(), URI: r.Base + "/s/q2", AccountID: alice.ID, Visibility: "public", Content: "<p>no</p>", CreatedAt: time.Now(), QuotePolicy: "nobody"}
	la.insertStatus(closed)
	if _, err := la.PostStatus(ctx, bob, PostInput{Text: "nope", Visibility: "public", QuoteOf: closed.ID}); err == nil {
		t.Fatal("quoted a post that forbids it")
	}
}

func TestLinkCards(t *testing.T) {
	la, _, bob, _ := featureFixture(t)
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html><html><head><title>Fallback</title>
			<meta property="og:title" content="Bread &amp; Butter">
			<meta property="og:description" content="A blog about baking">
			<meta property="og:image" content="/img/cover.jpg">
			<meta property="og:site_name" content="Crumbs"></head><body>ignored</body></html>`)
	}))
	defer page.Close()
	st, _ := la.PostStatus(context.Background(), bob, PostInput{Text: "read this " + page.URL + "/post #bread", Visibility: "public"})
	var full *Status
	eventually(t, "link card fetched", func() bool { full, _ = la.loadStatus(bob, st.ID); return full.Card != nil })
	c := full.Card
	u, _ := url.Parse(page.URL)
	if c.Title != "Bread & Butter" || c.Description != "A blog about baking" || c.Site != "Crumbs" || c.Image != "http://"+u.Host+"/img/cover.jpg" {
		t.Fatalf("card = %+v", c)
	}
	if firstLink(`<a href="https://x.test/@a" class="u-url mention">@a</a> <a href="https://x.test/tags/b" class="mention hashtag">#b</a> <a href="https://site.test/">s</a>`, "") != "https://site.test/" {
		t.Error("firstLink picked a mention or hashtag")
	}
}

func TestCustomEmoji(t *testing.T) {
	la, r, bob, _ := featureFixture(t)
	if err := la.AddCustomEmoji(":blob_pig:", pngBytes()); err != nil {
		t.Fatal(err)
	}
	if la.AddCustomEmoji("x", pngBytes()) == nil || la.AddCustomEmoji("ok_name", []byte("not an image")) == nil {
		t.Error("invalid emoji accepted")
	}
	st, err := la.PostStatus(context.Background(), bob, PostInput{Text: "oink :blob_pig: :unknown:", Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Emojis) != 1 || st.Emojis[0].Shortcode != "blob_pig" {
		t.Fatalf("emojis = %+v", st.Emojis)
	}
	if html := string(la.renderHTML(st.Content, nil, st.Emojis)); !strings.Contains(html, `class="emoji"`) || !strings.Contains(html, ":unknown:") {
		t.Errorf("rendered = %s", html)
	}
	eventually(t, "Create with Emoji tag", func() bool {
		for _, c := range r.receivedOf("Create") {
			for _, tag := range objsOf(objOf(c.Activity["object"])["tag"]) {
				if typeOf(tag) == "Emoji" && str(tag, "name") == ":blob_pig:" && strings.HasPrefix(urlOf(objOf(tag["icon"])), la.base+"/media/") {
					return true
				}
			}
		}
		return false
	})
	// names and bios
	bob.DisplayName = "Bob :blob_pig:"
	la.UpdateProfile(bob)
	_, actor := r.signedGet(bob.URI, "")
	if tags := objsOf(actor["tag"]); len(tags) != 1 || str(tags[0], "name") != ":blob_pig:" {
		t.Errorf("actor tags = %v", actor["tag"])
	}
	code, em := r.signedGet(la.base+"/emojis/blob_pig", "")
	if code != 200 || typeOf(em) != "Emoji" {
		t.Errorf("emoji object = %d %v", code, em)
	}
	c := &apiClient{t: t, base: la.base}
	if list := c.ok("GET", "/api/v1/custom_emojis", nil).([]any); len(list) != 1 || list[0].(map[string]any)["shortcode"] != "blob_pig" {
		t.Errorf("custom_emojis = %v", list)
	}
}

func TestRemoteQuotePolicy(t *testing.T) {
	a := &Account{URI: "https://m.test/users/a", FollowersURI: "https://m.test/users/a/followers"}
	policy := func(auto, manual []any) obj {
		return obj{"interactionPolicy": obj{"canQuote": obj{"automaticApproval": auto, "manualApproval": manual}}}
	}
	for _, c := range []struct {
		note obj
		want string
	}{
		{obj{}, ""},
		{policy([]any{asPublic}, nil), "public"},
		{policy(nil, []any{"as:Public"}), "public"},
		{policy([]any{a.FollowersURI}, nil), "followers"},
		{policy([]any{a.URI}, nil), "nobody"},
		{obj{"interactionPolicy": obj{"canQuote": obj{"automaticApproval": a.URI}}}, "nobody"},
	} {
		if got := remoteQuotePolicy(c.note, a); got != c.want {
			t.Errorf("%v: got %q, want %q", c.note, got, c.want)
		}
	}
}

// Edits notify those who boosted ("update") or quoted ("quoted_update")
// the post, as on Mastodon 4.5.
func TestEditNotifications(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	ctx := context.Background()
	key := r.Base + "/users/alice#main-key"
	nid := r.Base + "/notes/edited"
	note := obj{"id": nid, "type": "Note", "attributedTo": alice.URI, "content": "<p>first</p>", "to": []any{asPublic}, "published": apTime(time.Now())}
	r.send(obj{"@context": asContext, "id": nid + "/activity", "type": "Create", "actor": alice.URI, "to": []any{asPublic}, "object": note}, key, sigCavage)
	var theirs *Status
	eventually(t, "note stored", func() bool { theirs, _ = la.StatusByURI(nid); return theirs != nil })
	full, _ := la.loadStatus(bob, theirs.ID)
	if err := la.Boost(bob, full); err != nil {
		t.Fatal(err)
	}
	q, err := la.PostStatus(ctx, bob, PostInput{Text: "quoting", Visibility: "public", QuoteOf: theirs.ID})
	if err != nil {
		t.Fatal(err)
	}
	count := func(typ, statusID string) int {
		return la.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=? AND type=? AND status_id=?`, bob.ID, typ, statusID)
	}
	note["content"] = "<p>second</p>"
	r.send(obj{"@context": asContext, "id": nid + "#u1", "type": "Update", "actor": alice.URI, "object": note}, key, sigCavage)
	eventually(t, "edit notifications", func() bool { return count("update", theirs.ID) == 1 && count("quoted_update", q.ID) == 1 })
	la.db.Exec(`DELETE FROM notifications`)
	r.send(obj{"@context": asContext, "id": nid + "#u2", "type": "Update", "actor": alice.URI, "object": note}, key, sigCavage)
	time.Sleep(300 * time.Millisecond)
	if n := la.countInt(`SELECT COUNT(*) FROM notifications`); n != 0 {
		t.Fatalf("unchanged Update notified %d times", n)
	}

	// local edits notify local boosters
	eve, _ := la.CreateLocalUser("eve", "password123", false)
	mine, _ := la.PostStatus(ctx, bob, PostInput{Text: "draft", Visibility: "public"})
	mf, _ := la.loadStatus(eve, mine.ID)
	la.Boost(eve, mf)
	mf, _ = la.loadStatus(bob, mine.ID)
	if _, err := la.EditStatus(ctx, bob, mf, "final", "", false); err != nil {
		t.Fatal(err)
	}
	if n := la.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=? AND type='update'`, eve.ID); n != 1 {
		t.Fatalf("local update notifications = %d", n)
	}
}
