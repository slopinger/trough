package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"strings"
	"testing"
)

// groupActor returns a Lemmy-style community actor.
const lemmyGroup = `{
 "@context": ["https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1", {"lemmy": "https://join-lemmy.org/ns#", "litepub": "http://litepub.social/ns#", "pt": "https://joinpeertube.org/ns#", "sc": "http://schema.org/", "ChatMessage": "litepub:ChatMessage", "commentsEnabled": "pt:commentsEnabled", "sensitive": "as:sensitive", "matrixUserId": "lemmy:matrixUserId", "postingRestrictedToMods": "lemmy:postingRestrictedToMods", "removeData": "lemmy:removeData", "stickied": "lemmy:stickied", "moderators": {"@type": "@id", "@id": "lemmy:moderators"}, "expires": "as:endTime", "distinguished": "lemmy:distinguished", "language": "sc:inLanguage", "identifier": "sc:identifier"}],
 "type": "Group", "id": "{{R}}/c/cats", "preferredUsername": "cats", "name": "Cats", "summary": "<p>All about cats</p>",
 "source": {"content": "All about cats", "mediaType": "text/markdown"}, "sensitive": false,
 "attributedTo": "{{R}}/c/cats/moderators", "postingRestrictedToMods": false,
 "outbox": "{{R}}/c/cats/outbox", "followers": "{{R}}/c/cats/followers", "featured": "{{R}}/c/cats/featured",
 "inbox": "{{R}}/c/cats/inbox", "endpoints": {"sharedInbox": "{{R}}/inbox"},
 "publicKey": {"id": "{{R}}/c/cats#main-key", "owner": "{{R}}/c/cats", "publicKeyPem": "{{PEM}}"},
 "language": [{"identifier": "en", "name": "English"}], "published": "2023-06-01T00:00:00Z"
}`

const lemmyPerson = `{
 "@context": ["https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1"],
 "type": "Person", "id": "{{R}}/u/alice", "preferredUsername": "alice", "name": "alice",
 "inbox": "{{R}}/u/alice/inbox", "outbox": "{{R}}/u/alice/outbox", "endpoints": {"sharedInbox": "{{R}}/inbox"},
 "publicKey": {"id": "{{R}}/u/alice#main-key", "owner": "{{R}}/u/alice", "publicKeyPem": "{{PEM}}"},
 "published": "2023-06-01T00:00:00Z"
}`

const lemmyPage = `{
 "type": "Page", "id": "{{R}}/post/1", "attributedTo": "{{R}}/u/alice",
 "to": ["{{R}}/c/cats", "https://www.w3.org/ns/activitystreams#Public"], "name": "Look at this cat",
 "cc": [], "content": "<p>My cat is <strong>great</strong></p>\n", "mediaType": "text/html",
 "source": {"content": "My cat is **great**", "mediaType": "text/markdown"},
 "attachment": [{"href": "https://cats.example/article", "type": "Link"}],
 "image": {"type": "Image", "url": "{{R}}/pictrs/image/thumb.jpg"},
 "sensitive": false, "published": "2026-01-01T12:00:00Z", "language": {"identifier": "en", "name": "English"},
 "audience": "{{R}}/c/cats"
}`

func TestCompatLemmyCommunity(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	r := newRemote(t, la)
	r.put("/c/cats", lemmyGroup)
	r.put("/u/alice", lemmyPerson)
	r.put("/post/1", lemmyPage)
	group, err := la.getAccount(context.Background(), r.Base+"/c/cats")
	if err != nil {
		t.Fatal(err)
	}
	if group.ActorType != "Group" {
		t.Fatalf("type = %s", group.ActorType)
	}
	la.Follow(bob, group)
	eventually(t, "Follow delivered", func() bool { return len(r.receivedOf("Follow")) == 1 })
	gkey := r.Base + "/c/cats#main-key"
	r.send(obj{"@context": asContext, "id": r.Base + "/activities/accept/1", "type": "Accept", "actor": group.URI,
		"object": r.receivedOf("Follow")[0].Activity}, gkey, sigCavage)
	eventually(t, "community follow accepted", func() bool { return la.relationship(bob.ID, group.ID).Following })

	page := mustJSON(t, r.expand(lemmyPage))
	create := obj{"id": r.Base + "/activities/create/1", "type": "Create", "actor": r.Base + "/u/alice",
		"to": []any{asPublic}, "cc": []any{group.URI}, "object": page, "audience": group.URI}
	announce := obj{"@context": asContext, "id": r.Base + "/activities/announce/1", "type": "Announce", "actor": group.URI,
		"to": []any{asPublic}, "cc": []any{group.URI + "/followers"}, "object": create}
	if code := r.send(announce, gkey, sigCavage); code != 202 {
		t.Fatalf("announce rejected: %d", code)
	}
	var st *Status
	eventually(t, "page stored", func() bool { st, err = la.StatusByURI(r.Base + "/post/1"); return err == nil })
	if !strings.Contains(st.Content, "Look at this cat") || !strings.Contains(st.Content, "https://cats.example/article") {
		t.Errorf("page title/link missing from content: %s", st.Content)
	}
	author, _ := la.AccountByID(st.AccountID)
	if author.URI != r.Base+"/u/alice" {
		t.Errorf("page attributed to %s", author.URI)
	}
	eventually(t, "community post in home timeline as a boost by the group", func() bool {
		home, _ := la.homeTimeline(bob, "")
		return len(home) == 1 && home[0].BoostOfID == st.ID && home[0].AccountID == group.ID
	})
	// a comment, announced the same way
	r.put("/comment/1", `{"type": "Note", "id": "{{R}}/comment/1", "attributedTo": "{{R}}/u/alice", "to": ["https://www.w3.org/ns/activitystreams#Public"], "cc": ["{{R}}/c/cats"], "content": "<p>first!</p>", "inReplyTo": "{{R}}/post/1", "published": "2026-01-01T12:05:00Z", "audience": "{{R}}/c/cats"}`)
	note := mustJSON(t, r.expand(r.docs["/comment/1"]))
	r.send(obj{"@context": asContext, "id": r.Base + "/activities/announce/2", "type": "Announce", "actor": group.URI, "to": []any{asPublic},
		"object": obj{"id": r.Base + "/activities/create/2", "type": "Create", "actor": r.Base + "/u/alice", "object": note}}, gkey, sigCavage)
	eventually(t, "comment stored in thread", func() bool {
		c, err := la.StatusByURI(r.Base + "/comment/1")
		return err == nil && c.InReplyToID == st.ID
	})
	// the community relays a deletion
	r.put("/comment/1", "GONE")
	r.send(obj{"@context": asContext, "id": r.Base + "/activities/announce/3", "type": "Announce", "actor": group.URI, "to": []any{asPublic},
		"object": obj{"id": r.Base + "/activities/delete/1", "type": "Delete", "actor": r.Base + "/u/alice", "object": r.Base + "/comment/1"}}, gkey, sigCavage)
	eventually(t, "relayed delete applied", func() bool { _, err := la.StatusByURI(r.Base + "/comment/1"); return err != nil })
}

const peertubeAccount = `{
 "type": "Person", "id": "{{R}}/accounts/alice", "following": "{{R}}/accounts/alice/following", "followers": "{{R}}/accounts/alice/followers",
 "playlists": "{{R}}/accounts/alice/playlists", "inbox": "{{R}}/accounts/alice/inbox", "outbox": "{{R}}/accounts/alice/outbox",
 "preferredUsername": "alice", "url": "{{R}}/accounts/alice", "name": "Alice", "endpoints": {"sharedInbox": "{{R}}/inbox"},
 "publicKey": {"id": "{{R}}/accounts/alice#main-key", "owner": "{{R}}/accounts/alice", "publicKeyPem": "{{PEM}}"},
 "published": "2022-01-01T00:00:00.000Z", "summary": null,
 "@context": ["https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1", {"RsaSignature2017": "https://w3id.org/security#RsaSignature2017"}]
}`

const peertubeChannel = `{
 "type": "Group", "id": "{{R}}/video-channels/alice_channel", "following": "{{R}}/video-channels/alice_channel/following",
 "followers": "{{R}}/video-channels/alice_channel/followers", "playlists": "{{R}}/video-channels/alice_channel/playlists",
 "inbox": "{{R}}/video-channels/alice_channel/inbox", "outbox": "{{R}}/video-channels/alice_channel/outbox",
 "preferredUsername": "alice_channel", "url": "{{R}}/video-channels/alice_channel", "name": "Alice's videos",
 "endpoints": {"sharedInbox": "{{R}}/inbox"},
 "publicKey": {"id": "{{R}}/video-channels/alice_channel#main-key", "owner": "{{R}}/video-channels/alice_channel", "publicKeyPem": "{{PEM}}"},
 "published": "2022-01-01T00:00:00.000Z", "summary": "Videos!", "support": null, "postingRestrictedToMods": true,
 "attributedTo": [{"type": "Person", "id": "{{R}}/accounts/alice"}],
 "@context": ["https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1"]
}`

const peertubeVideo = `{
 "type": "Video", "id": "{{R}}/videos/watch/9c9de5e8", "name": "My cat video", "duration": "PT33S", "uuid": "9c9de5e8",
 "category": {"identifier": "15", "name": "Science & Technology"}, "views": 3, "sensitive": false,
 "waitTranscoding": false, "state": 1, "commentsEnabled": true, "downloadEnabled": true,
 "published": "2026-01-01T12:00:00.000Z", "updated": "2026-01-01T12:00:00.000Z",
 "mediaType": "text/markdown", "content": "A **cat** doing things.\n\nSecond paragraph.",
 "support": null, "subtitleLanguage": [],
 "icon": [{"type": "Image", "url": "{{R}}/lazy-static/thumbnails/small.jpg", "mediaType": "image/jpeg", "width": 280, "height": 157},
   {"type": "Image", "url": "{{R}}/lazy-static/previews/big.jpg", "mediaType": "image/jpeg", "width": 850, "height": 480}],
 "url": [{"type": "Link", "mediaType": "text/html", "href": "{{R}}/w/kkGMgK9ZtnKfYAgnEtQxbv"},
   {"type": "Link", "mediaType": "video/mp4", "href": "{{R}}/static/web-videos/9c9de5e8-720.mp4", "height": 720, "size": 1234567, "fps": 30}],
 "likes": "{{R}}/videos/watch/9c9de5e8/likes", "dislikes": "{{R}}/videos/watch/9c9de5e8/dislikes",
 "shares": "{{R}}/videos/watch/9c9de5e8/announces", "comments": "{{R}}/videos/watch/9c9de5e8/comments",
 "attributedTo": [{"type": "Person", "id": "{{R}}/accounts/alice"}, {"type": "Group", "id": "{{R}}/video-channels/alice_channel"}],
 "to": ["https://www.w3.org/ns/activitystreams#Public"], "cc": ["{{R}}/accounts/alice/followers"]
}`

func TestCompatPeerTubeVideo(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	r := newRemote(t, la)
	r.put("/accounts/alice", peertubeAccount)
	r.put("/video-channels/alice_channel", peertubeChannel)
	r.put("/videos/watch/9c9de5e8", peertubeVideo)
	ch, err := la.getAccount(context.Background(), r.Base+"/video-channels/alice_channel")
	if err != nil {
		t.Fatal(err)
	}
	la.Follow(bob, ch)
	eventually(t, "Follow delivered", func() bool { return len(r.receivedOf("Follow")) == 1 })
	ckey := r.Base + "/video-channels/alice_channel#main-key"
	r.send(obj{"@context": asContext, "id": r.Base + "/accepts/1", "type": "Accept", "actor": ch.URI, "object": r.receivedOf("Follow")[0].Activity}, ckey, sigCavage)
	eventually(t, "channel follow accepted", func() bool { return la.relationship(bob.ID, ch.ID).Following })

	video := mustJSON(t, r.expand(peertubeVideo))
	r.send(obj{"@context": asContext, "id": r.Base + "/videos/watch/9c9de5e8/activity", "type": "Create", "actor": ch.URI,
		"to": video["to"], "cc": video["cc"], "object": video}, ckey, sigCavage)
	var st *Status
	eventually(t, "video stored", func() bool { st, err = la.StatusByURI(r.Base + "/videos/watch/9c9de5e8"); return err == nil })
	full, _ := la.loadStatus(bob, st.ID)
	if !strings.Contains(full.Content, "My cat video") || !strings.Contains(full.Content, "<strong>cat</strong>") {
		t.Errorf("video title/markdown description missing: %s", full.Content)
	}
	if full.URL != r.Base+"/w/kkGMgK9ZtnKfYAgnEtQxbv" {
		t.Errorf("url = %s", full.URL)
	}
	if len(full.Attachments) != 1 || !strings.HasSuffix(full.Attachments[0].URL, "big.jpg") {
		t.Errorf("thumbnail not attached: %+v", full.Attachments)
	}
	eventually(t, "video in home timeline", func() bool { home, _ := la.homeTimeline(bob, ""); return len(home) == 1 })
}

// Servers fetch our objects, sometimes with signatures (authorized fetch).
func TestCompatAuthorizedFetch(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	r := newRemote(t, la)
	r.put("/users/alice", mastodonActor)
	r.put("/users/eve", strings.ReplaceAll(strings.ReplaceAll(mastodonActor, "users/alice", "users/eve"), `"alice"`, `"eve"`))
	alice, err := la.getAccount(context.Background(), r.Base+"/users/alice")
	if err != nil {
		t.Fatal(err)
	}
	la.db.Exec(`INSERT INTO follows (id,uri,account_id,target_id,accepted,created_at) VALUES ('f1','x',?,?,1,0)`, alice.ID, bob.ID)
	pub, _ := la.PostStatus(context.Background(), bob, PostInput{Text: "public", Visibility: "public"})
	priv, _ := la.PostStatus(context.Background(), bob, PostInput{Text: "followers only", Visibility: "private"})

	if code, m := r.signedGet(pub.URI, ""); code != 200 || str(m, "id") != pub.URI {
		t.Errorf("unsigned fetch of public post: %d", code)
	}
	if code, _ := r.signedGet(priv.URI, ""); code != 404 {
		t.Errorf("unsigned fetch of private post: %d, want 404", code)
	}
	if code, _ := r.signedGet(priv.URI, r.Base+"/users/eve#main-key"); code != 404 {
		t.Errorf("non-follower fetch of private post: %d, want 404", code)
	}
	if code, m := r.signedGet(priv.URI, r.Base+"/users/alice#main-key"); code != 200 || str(m, "content") == "" {
		t.Errorf("follower fetch of private post: %d", code)
	}
	// the HTML URL serves ActivityPub too (Mastodon's search pastes those)
	if code, m := r.signedGet(pub.URL, ""); code != 200 || str(m, "id") != pub.URI {
		t.Errorf("fetch of html url: %d", code)
	}
}

// Shape checks for the documents other servers depend on.
func TestCompatOurDocuments(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	get := func(u, accept string) (int, obj, http.Header) {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Accept", accept)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m obj
		json.Unmarshal(b, &m)
		return resp.StatusCode, m, resp.Header
	}
	// WebFinger
	code, wf, h := get(la.base+"/.well-known/webfinger?resource=acct:bob@"+la.host(), "application/jrd+json")
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "application/jrd+json") || str(wf, "subject") != "acct:bob@"+la.host() {
		t.Fatalf("webfinger: %d %v", code, wf)
	}
	self := ""
	for _, l := range objsOf(wf["links"]) {
		if str(l, "rel") == "self" && str(l, "type") == apMimeType {
			self = str(l, "href")
		}
	}
	if self != bob.URI {
		t.Fatalf("webfinger self link = %q", self)
	}
	// actor, with both accept headers servers use
	for _, accept := range []string{apMimeType, ldMimeType} {
		code, actor, h := get(bob.URI, accept)
		if code != 200 || !strings.Contains(h.Get("Content-Type"), "activity+json") {
			t.Fatalf("actor with %s: %d %s", accept, code, h.Get("Content-Type"))
		}
		for _, k := range []string{"id", "type", "inbox", "outbox", "followers", "following", "preferredUsername", "publicKey", "endpoints"} {
			if actor[k] == nil {
				t.Errorf("actor lacks %s", k)
			}
		}
		pk := objOf(actor["publicKey"])
		if str(pk, "owner") != bob.URI || !strings.HasPrefix(str(pk, "id"), bob.URI) {
			t.Errorf("bad publicKey: %v", pk)
		}
		ctx, _ := actor["@context"].([]any)
		if len(ctx) < 2 || ctx[0] != asContext || ctx[1] != "https://w3id.org/security/v1" {
			t.Errorf("actor @context = %v", actor["@context"])
		}
	}
	// browsers get HTML
	req, _ := http.NewRequest("GET", bob.URI, nil)
	req.Header.Set("Accept", "text/html")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("browser got %s", resp.Header.Get("Content-Type"))
	}
	// discovery documents are readable by browser-based clients (Phanpy, Elk)
	for _, path := range []string{"/static/avatar.svg", "/.well-known/host-meta", "/.well-known/nodeinfo", "/.well-known/webfinger?resource=acct:bob@" + la.host()} {
		req, _ := http.NewRequest("GET", la.base+path, nil)
		req.Header.Set("Origin", "https://phanpy.social")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s lacks CORS", path)
		}
	}
	// NodeInfo
	code, ni, _ := get(la.base+"/.well-known/nodeinfo", "application/json")
	if code != 200 || len(objsOf(ni["links"])) == 0 {
		t.Fatalf("nodeinfo index: %d", code)
	}
	code, n2, _ := get(str(objsOf(ni["links"])[0], "href"), "application/json")
	if code != 200 || str(objOf(n2["software"]), "name") != softwareName {
		t.Fatalf("nodeinfo: %d %v", code, n2)
	}
	// outbox paging
	for i := 0; i < 3; i++ {
		la.PostStatus(context.Background(), bob, PostInput{Text: "post", Visibility: "public"})
	}
	code, ob, _ := get(bob.URI+"/outbox", apMimeType)
	if code != 200 || typeOf(ob) != "OrderedCollection" || ob["totalItems"].(float64) != 3 {
		t.Fatalf("outbox: %d %v", code, ob)
	}
	code, page, _ := get(str(ob, "first"), apMimeType)
	items, _ := page["orderedItems"].([]any)
	if code != 200 || len(items) != 3 || typeOf(objOf(items[0])) != "Create" {
		t.Fatalf("outbox page: %d %v", code, page)
	}
	// replies: an embedded first page (GoToSocial needs one) whose next link
	// is fetchable
	note := objOf(objOf(items[2])["object"])
	reply, _ := la.PostStatus(context.Background(), bob, PostInput{Text: "reply", Visibility: "public", InReplyTo: path.Base(str(note, "id"))})
	replies := objOf(note["replies"])
	first := objOf(replies["first"])
	if typeOf(first) != "CollectionPage" || str(first, "partOf") != str(replies, "id") {
		t.Fatalf("replies = %v", replies)
	}
	priv, _ := la.PostStatus(context.Background(), bob, PostInput{Text: "secret", Visibility: "private"})
	if privFull, _ := la.loadStatus(bob, priv.ID); la.noteJSON(privFull)["replies"] != nil {
		t.Error("followers-only post links a replies collection that 404s")
	}
	code, rp, _ := get(str(first, "next"), apMimeType)
	if ri, _ := rp["items"].([]any); code != 200 || typeOf(rp) != "CollectionPage" || len(ri) != 1 || ri[0] != reply.URI {
		t.Fatalf("replies page %v: %d %v", replies, code, rp)
	}
}

// A server that signs with RFC 9421 but rejects it from us gets draft-cavage
// after one 401 and keeps getting it.
func TestCompatRFC9421Fallback(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	r := newRemote(t, la)
	r.RejectRFC9421 = true
	r.put("/@alice", fedifyActor)
	keyID := r.Base + "/@alice#main-key"
	follow := func(n string) {
		t.Helper()
		if code := r.send(obj{"@context": asContext, "id": r.Base + "/follows/" + n, "type": "Follow", "actor": r.Base + "/@alice", "object": bob.URI}, keyID, sigRFC9421); code != 202 {
			t.Fatalf("follow rejected: %d", code)
		}
	}
	follow("1")
	eventually(t, "Accept delivered", func() bool { return len(r.receivedOf("Accept")) == 1 })
	acc := r.receivedOf("Accept")[0]
	if acc.SigErr != nil || acc.Header.Get("Signature-Input") != "" || acc.Header.Get("Signature") == "" {
		t.Fatalf("fallback Accept: sig err %v, headers %v", acc.SigErr, acc.Header)
	}
	if r.rejected() != 1 || la.sigStyle(r.Host) != sigStyleCavage {
		t.Fatalf("rejected %d, style %q", r.rejected(), la.sigStyle(r.Host))
	}
	// their next RFC 9421 request doesn't make us try again right away
	follow("2")
	eventually(t, "second Accept delivered", func() bool { return len(r.receivedOf("Accept")) == 2 })
	if n := r.rejected(); n != 1 {
		t.Errorf("retried RFC 9421 against a host that rejected it: %d rejections", n)
	}
}

// Signed fetches follow the same per-host choice as deliveries.
func TestCompatRFC9421Fetch(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	r := newRemote(t, la)
	r.put("/@alice", fedifyActor)
	r.put("/notes/1", `{"id": "{{R}}/notes/1", "type": "Note", "attributedTo": "{{R}}/@alice", "content": "hi", "to": ["https://www.w3.org/ns/activitystreams#Public"]}`)
	if code := r.send(obj{"@context": asContext, "id": r.Base + "/follows/1", "type": "Follow", "actor": r.Base + "/@alice", "object": bob.URI}, r.Base+"/@alice#main-key", sigRFC9421); code != 202 {
		t.Fatalf("follow: %d", code)
	}
	if _, err := la.fetchAP(context.Background(), r.Base+"/notes/1"); err != nil {
		t.Fatal(err)
	}
	lastGet := func(r *remote, path string) *http.Request {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i := len(r.gets) - 1; i >= 0; i-- {
			if r.gets[i].Method == "GET" && r.gets[i].URL.Path == path {
				return r.gets[i]
			}
		}
		return nil
	}
	get := lastGet(r, "/notes/1")
	if get == nil || get.Header.Get("Signature-Input") == "" {
		t.Fatalf("fetch not signed with RFC 9421: %v", get)
	}
	if err := r.verifyIncomingRFC9421(get, nil); err != nil {
		t.Fatalf("our RFC 9421 GET signature: %v", err)
	}

	// a host that refuses it gets draft-cavage on the retry
	r2 := newRemote(t, la)
	r2.RejectRFC9421 = true
	r2.put("/notes/2", `{"id": "{{R}}/notes/2", "type": "Note", "attributedTo": "{{R}}/@alice", "content": "hi"}`)
	la.sawRFC9421(r2.Host)
	if _, err := la.fetchAP(context.Background(), r2.Base+"/notes/2"); err != nil {
		t.Fatalf("fallback fetch: %v", err)
	}
	if r2.rejected() != 1 || la.sigStyle(r2.Host) != sigStyleCavage {
		t.Fatalf("rejected %d, style %s", r2.rejected(), la.sigStyle(r2.Host))
	}
	if get := lastGet(r2, "/notes/2"); get == nil || get.Header.Get("Signature") == "" || get.Header.Get("Signature-Input") != "" {
		t.Fatalf("retry not draft-cavage: %v", get)
	}
}
