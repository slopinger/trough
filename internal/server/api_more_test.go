package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Endpoints found missing by comparing with the open-source iOS clients
// (Ice Cubes and the official Mastodon app).

func TestAPIReport(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	id := newID()
	la.insertStatus(&Status{ID: id, URI: r.Base + "/s/bad", AccountID: alice.ID, Visibility: "public", Content: "<p>spam</p>", CreatedAt: parseTime("2026-01-01T00:00:00Z")})
	rep := c.ok("POST", "/api/v1/reports", url.Values{"account_id": {alice.ID}, "status_ids[]": {id, "nope"}, "comment": {"spam"},
		"forward": {"true"}, "category": {"spam"}}).(map[string]any)
	if rep["forwarded"] != true || rep["category"] != "spam" || len(rep["status_ids"].([]any)) != 1 || rep["target_account"].(map[string]any)["id"] != alice.ID {
		t.Fatalf("report = %v", rep)
	}
	eventually(t, "Flag delivered", func() bool { return len(r.receivedOf("Flag")) == 1 })
	if len(la.openReports()) != 1 {
		t.Error("report not stored")
	}
	if code, _, _ := c.do("POST", "/api/v1/reports", map[string]any{"account_id": bob.ID}); code != 422 {
		t.Errorf("reporting yourself: %d", code)
	}
	if code, _, _ := c.do("POST", "/api/v1/reports", map[string]any{"account_id": "missing"}); code != 404 {
		t.Errorf("unknown account: %d", code)
	}
}

func TestAPIDeleteProfileImages(t *testing.T) {
	la, r, bob, _ := featureFixture(t)
	token := oauthLogin(t, la, "bob", "password123", "read write")
	c := &apiClient{t: t, base: la.base, token: token}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range []string{"avatar", "header"} {
		fw, _ := mw.CreateFormFile(f, f+".png")
		fw.Write(pngBytes())
	}
	mw.Close()
	req, _ := http.NewRequest("PATCH", la.base+"/api/v1/accounts/update_credentials", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(err, resp.StatusCode)
	}
	resp.Body.Close()
	if me, _ := la.AccountByID(bob.ID); !strings.Contains(me.Avatar, "/media/") || !strings.Contains(me.Header, "/media/") {
		t.Fatalf("images not set: %q %q", me.Avatar, me.Header)
	}
	acc := c.ok("DELETE", "/api/v1/profile/avatar", nil).(map[string]any)
	if strings.Contains(acc["avatar"].(string), "/media/") {
		t.Errorf("avatar still %v", acc["avatar"])
	}
	c.ok("DELETE", "/api/v1/profile/header", nil)
	if me, _ := la.AccountByID(bob.ID); me.Avatar != "" || me.Header != "" {
		t.Errorf("images left: %q %q", me.Avatar, me.Header)
	}
	if code, _, _ := c.do("DELETE", "/api/v1/profile/banner", nil); code != 404 {
		t.Errorf("unknown image: %d", code)
	}
	eventually(t, "profile Update federated", func() bool { return len(r.receivedOf("Update")) >= 3 })
}

func TestAPIStatusQuotes(t *testing.T) {
	la, _, bob, alice := featureFixture(t)
	ctx := context.Background()
	eve, _ := la.CreateLocalUser("eve", "password123", false)
	orig, _ := la.PostStatus(ctx, bob, PostInput{Text: "quote me", Visibility: "public"})
	q1, _ := la.PostStatus(ctx, eve, PostInput{Text: "public quote", Visibility: "public", QuoteOf: orig.ID})
	la.PostStatus(ctx, eve, PostInput{Text: "private quote", Visibility: "private", QuoteOf: orig.ID})
	la.PostStatus(ctx, eve, PostInput{Text: "not a quote", Visibility: "public"})
	la.insertStatus(&Status{ID: newID(), URI: alice.URI + "/s/q", AccountID: alice.ID, Visibility: "public", Content: "<p>remote quote</p>",
		QuoteURI: orig.URI, CreatedAt: parseTime("2026-01-01T00:00:00Z")})
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read")}
	got := c.ok("GET", "/api/v1/statuses/"+orig.ID+"/quotes", nil).([]any)
	texts := []string{}
	for _, s := range got {
		texts = append(texts, plainText(s.(map[string]any)["content"].(string)))
	}
	if len(got) != 2 || got[1].(map[string]any)["id"] != q1.ID || !strings.HasPrefix(texts[0], "remote quote") {
		t.Fatalf("quotes = %q", texts)
	}
	if n := c.ok("GET", "/api/v1/statuses/"+orig.ID, nil).(map[string]any)["quotes_count"]; n != float64(3) {
		t.Fatalf("quotes_count = %v", n) // two public quotes and one private
	}
	if got := (&apiClient{t: t, base: la.base}).ok("GET", "/api/v1/statuses/"+orig.ID+"/quotes", nil).([]any); len(got) != 2 {
		t.Errorf("anonymous sees %d quotes", len(got))
	}
}

func TestAPILinkTimeline(t *testing.T) {
	la, _, bob, alice := featureFixture(t)
	link := "https://news.example/story"
	for i, vis := range []string{"public", "public", "private"} {
		la.insertStatus(&Status{ID: newID(), URI: alice.URI + "/s/" + string(rune('a'+i)), AccountID: alice.ID, Visibility: vis,
			Content: "<p>read this</p>", CardURL: link, CreatedAt: parseTime("2026-01-01T00:00:00Z")})
	}
	la.PostStatus(context.Background(), bob, PostInput{Text: "unrelated", Visibility: "public"})
	pub := &apiClient{t: t, base: la.base}
	if got := pub.ok("GET", "/api/v1/timelines/link?url="+url.QueryEscape(link), nil).([]any); len(got) != 2 {
		t.Errorf("link timeline has %d posts", len(got))
	}
	if code, _, _ := pub.do("GET", "/api/v1/timelines/link", nil); code != 422 {
		t.Errorf("no url: %d", code)
	}
}

func TestAPINotificationRequests(t *testing.T) {
	la, _, _, _ := featureFixture(t)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	for _, p := range []string{"/api/v1/notifications/requests/accept", "/api/v1/notifications/requests/dismiss"} {
		c.ok("POST", p, url.Values{"id[]": {"1"}})
	}
	if c.ok("GET", "/api/v1/notifications/requests/merged", nil).(map[string]any)["merged"] != true {
		t.Error("not merged")
	}
	for _, p := range []string{"GET /api/v1/notifications/requests/1", "POST /api/v1/notifications/requests/1/accept"} {
		m, path, _ := strings.Cut(p, " ")
		if code, _, _ := c.do(m, path, nil); code != 404 {
			t.Errorf("%s: %d", p, code)
		}
	}
}

func TestAPIConversations(t *testing.T) {
	la, _, bob, alice := featureFixture(t)
	ctx := context.Background()
	eve, _ := la.CreateLocalUser("eve", "password123", false)
	dm := func(from *Account, text, replyTo string) *Status {
		t.Helper()
		st, err := la.PostStatus(ctx, from, PostInput{Text: text, Visibility: "direct", InReplyTo: replyTo})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	first := dm(bob, "@eve hi", "")
	dm(eve, "@bob hey you", first.ID)
	other := dm(bob, "@"+alice.Acct()+" psst", "")
	la.PostStatus(ctx, bob, PostInput{Text: "public", Visibility: "public"})
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	list := func() []map[string]any {
		var out []map[string]any
		for _, v := range c.ok("GET", "/api/v1/conversations", nil).([]any) {
			out = append(out, v.(map[string]any))
		}
		return out
	}
	cs := list()
	if len(cs) != 2 || cs[0]["id"] != other.ID || cs[1]["id"] != first.ID {
		t.Fatalf("conversations = %v", cs)
	}
	thread := cs[1]
	if accs := thread["accounts"].([]any); len(accs) != 1 || accs[0].(map[string]any)["id"] != eve.ID {
		t.Errorf("participants = %v", accs)
	}
	if thread["unread"] != true || cs[0]["unread"] != false {
		t.Errorf("unread = %v / %v", thread["unread"], cs[0]["unread"])
	}
	if got := c.ok("POST", "/api/v1/conversations/"+first.ID+"/read", nil).(map[string]any); got["unread"] != false {
		t.Errorf("read = %v", got["unread"])
	}
	if list()[1]["unread"] != false {
		t.Error("still unread")
	}
	// paging by last status
	code, page, hdr := c.do("GET", "/api/v1/conversations?limit=1", nil)
	if code != 200 || len(page.([]any)) != 1 || !strings.Contains(hdr.Get("Link"), "max_id="+other.ID) {
		t.Errorf("page: %d %v %q", code, page, hdr.Get("Link"))
	}
	// hiding lasts until a new message
	c.ok("DELETE", "/api/v1/conversations/"+first.ID, nil)
	if cs := list(); len(cs) != 1 {
		t.Fatalf("after delete: %v", cs)
	}
	dm(eve, "@bob still there?", first.ID)
	if cs := list(); len(cs) != 2 || cs[0]["id"] != first.ID || cs[0]["unread"] != true {
		t.Errorf("after new message: %v", cs)
	}
	if code, _, _ := c.do("POST", "/api/v1/conversations/nope/read", nil); code != 404 {
		t.Errorf("unknown conversation: %d", code)
	}
	// eve sees the same thread from her side
	ce := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "eve", "password123", "read")}
	if cs := ce.ok("GET", "/api/v1/conversations", nil).([]any); len(cs) != 1 || cs[0].(map[string]any)["id"] != first.ID {
		t.Errorf("eve's conversations = %v", cs)
	}
}

func TestAPIFilters(t *testing.T) {
	la, _, bob, alice := featureFixture(t)
	ctx := context.Background()
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	post := func(text string) *Status {
		st := &Status{ID: newID(), URI: alice.URI + "/s/" + newID(), AccountID: alice.ID, Visibility: "public", Content: "<p>" + text + "</p>", CreatedAt: parseTime("2026-01-01T00:00:00Z")}
		la.insertStatus(st)
		return st
	}
	spoiler := post("big spoilers for the finale")
	ids := func(path string) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, v := range c.ok("GET", path, nil).([]any) {
			m := v.(map[string]any)
			out[m["id"].(string)] = m
		}
		return out
	}

	// v2, JSON with keywords
	f := c.ok("POST", "/api/v2/filters", map[string]any{"title": "No spoilers", "context": []string{"home"}, "filter_action": "hide",
		"expires_in": "3600", "keywords_attributes": []any{map[string]any{"keyword": "spoilers", "whole_word": true}, map[string]any{"keyword": "finale"}}}).(map[string]any)
	fid := f["id"].(string)
	if len(f["keywords"].([]any)) != 2 || f["expires_at"] == nil || f["filter_action"] != "hide" || len(f["context"].([]any)) != 1 {
		t.Fatalf("created %v", f)
	}
	if _, ok := ids("/api/v1/timelines/home")[spoiler.ID]; ok {
		t.Error("hidden post in home")
	}
	if _, ok := ids("/api/v1/timelines/public")[spoiler.ID]; !ok {
		t.Error("filter applied outside its context")
	}

	// update with Rails-style form fields: drop one keyword, change one, add one
	kws := f["keywords"].([]any)
	k0, k1 := kws[0].(map[string]any)["id"].(string), kws[1].(map[string]any)["id"].(string)
	f = c.ok("PUT", "/api/v2/filters/"+fid, url.Values{"filter_action": {"warn"}, "context[]": {"home", "public"},
		"keywords_attributes[][id]": {k0, k1, ""}, "keywords_attributes[][keyword]": {"spoiler", "", "ending"},
		"keywords_attributes[][_destroy]": {"false", "true", "false"}}).(map[string]any)
	var words []string
	for _, k := range f["keywords"].([]any) {
		words = append(words, k.(map[string]any)["keyword"].(string))
	}
	if strings.Join(words, ",") != "spoiler,ending" || f["filter_action"] != "warn" {
		t.Fatalf("updated keywords %v, %v", words, f["filter_action"])
	}
	post("the ending was great")
	home := ids("/api/v1/timelines/home")
	if res := home[spoiler.ID]; res == nil || len(res["filtered"].([]any)) != 0 {
		t.Errorf("'spoiler' is a whole word and shouldn't match 'spoilers': %v", res["filtered"])
	}
	var warned map[string]any
	for _, s := range home {
		if strings.Contains(s["content"].(string), "ending") {
			warned = s
		}
	}
	if fr := warned["filtered"].([]any); len(fr) != 1 || fr[0].(map[string]any)["filter"].(map[string]any)["id"] != fid ||
		fr[0].(map[string]any)["keyword_matches"].([]any)[0] != "ending" {
		t.Errorf("filter result = %v", warned["filtered"])
	}

	// keyword endpoints (Ice Cubes sends these as query parameters)
	k := c.ok("POST", "/api/v2/filters/"+fid+"/keywords?keyword=finale&whole_word=false", nil).(map[string]any)
	kid := k["id"].(string)
	if k["whole_word"] != false || len(c.ok("GET", "/api/v2/filters/"+fid+"/keywords", nil).([]any)) != 3 {
		t.Errorf("keyword added: %v", k)
	}
	if got := c.ok("PUT", "/api/v2/filters/keywords/"+kid, url.Values{"keyword": {"finales"}}).(map[string]any); got["keyword"] != "finales" {
		t.Errorf("keyword update: %v", got)
	}
	if got := c.ok("GET", "/api/v2/filters/keywords/"+kid, nil).(map[string]any); got["keyword"] != "finales" {
		t.Errorf("keyword get: %v", got)
	}
	c.ok("DELETE", "/api/v2/filters/keywords/"+kid, nil)
	if len(c.ok("GET", "/api/v2/filters/"+fid, nil).(map[string]any)["keywords"].([]any)) != 2 {
		t.Error("keyword not deleted")
	}
	// an empty expires_in removes the expiry; expired filters stop applying
	if got := c.ok("PUT", "/api/v2/filters/"+fid, map[string]any{"expires_in": ""}).(map[string]any); got["expires_at"] != nil {
		t.Errorf("expiry kept: %v", got["expires_at"])
	}
	la.db.Exec(`UPDATE filter_groups SET expires_at=1 WHERE id=?`, fid)
	for _, s := range ids("/api/v1/timelines/home") {
		if len(s["filtered"].([]any)) != 0 {
			t.Error("expired filter applied")
		}
	}

	// thread context
	reply, _ := la.PostStatus(ctx, alice, PostInput{Text: "a thread reply with spoilers", Visibility: "public", InReplyTo: spoiler.ID})
	tf := c.ok("POST", "/api/v2/filters", map[string]any{"title": "threads", "context": []string{"thread"}, "filter_action": "hide",
		"keywords_attributes": []any{map[string]any{"keyword": "thread reply"}}}).(map[string]any)
	if d := c.ok("GET", "/api/v1/statuses/"+spoiler.ID+"/context", nil).(map[string]any)["descendants"].([]any); len(d) != 0 {
		t.Errorf("reply %s not hidden in thread: %v", reply.ID, d)
	}
	c.ok("DELETE", "/api/v2/filters/"+tf["id"].(string), nil)

	// someone else can't see or change them
	ce := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, func() string { la.CreateLocalUser("eve", "password123", false); return "eve" }(), "password123", "read write")}
	if code, _, _ := ce.do("GET", "/api/v2/filters/"+fid, nil); code != 404 {
		t.Errorf("eve reads bob's filter: %d", code)
	}
	if code, _, _ := ce.do("PUT", "/api/v2/filters/"+fid, map[string]any{"title": "x"}); code != 404 {
		t.Errorf("eve edits bob's filter: %d", code)
	}

	// v1: one keyword per filter, shown as a group in v2
	v1 := c.ok("POST", "/api/v1/filters", url.Values{"phrase": {"crypto"}, "context[]": {"public"}, "irreversible": {"true"}, "whole_word": {"false"}}).(map[string]any)
	if v1["irreversible"] != true || v1["whole_word"] != false || v1["phrase"] != "crypto" {
		t.Fatalf("v1 created %v", v1)
	}
	v1 = c.ok("PUT", "/api/v1/filters/"+v1["id"].(string), url.Values{"phrase": {"nft"}, "irreversible": {"false"}}).(map[string]any)
	if v1["phrase"] != "nft" || v1["irreversible"] != false {
		t.Errorf("v1 updated %v", v1)
	}
	if groups := c.ok("GET", "/api/v2/filters", nil).([]any); len(groups) != 2 {
		t.Errorf("v2 sees %d groups", len(groups))
	}
	c.ok("DELETE", "/api/v1/filters/"+v1["id"].(string), nil)
	c.ok("DELETE", "/api/v2/filters/"+fid, nil)
	if n := la.countInt(`SELECT COUNT(*) FROM filter_groups`) + la.countInt(`SELECT COUNT(*) FROM filters`); n != 0 {
		t.Errorf("%d filter rows left", n)
	}
	// filters from the web UI show up too
	la.AddFilter(bob, "politics", true, "warn")
	if got := c.ok("GET", "/api/v2/filters", nil).([]any); len(got) != 1 || got[0].(map[string]any)["title"] != "politics" {
		t.Errorf("web filter = %v", got)
	}
}

// Mastodon 4.5 quote settings: quote_approval on statuses, the policy when
// posting and later, and revoking a quote.
func TestAPIQuoteApproval(t *testing.T) {
	la, _, bob, _ := featureFixture(t)
	ctx := context.Background()
	eve, _ := la.CreateLocalUser("eve", "password123", false)
	bc := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	ec := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "eve", "password123", "read write")}
	approval := func(c *apiClient, id string) map[string]any {
		return c.ok("GET", "/api/v1/statuses/"+id, nil).(map[string]any)["quote_approval"].(map[string]any)
	}

	open := bc.ok("POST", "/api/v1/statuses", url.Values{"status": {"quote me"}}).(map[string]any)
	if a := approval(ec, open["id"].(string)); a["current_user"] != "automatic" || len(a["automatic"].([]any)) != 1 || a["automatic"].([]any)[0] != "public" {
		t.Fatalf("public post: %v", a)
	}
	if a := approval(&apiClient{t: t, base: la.base}, open["id"].(string)); a["current_user"] != "denied" {
		t.Fatalf("anonymous: %v", a)
	}
	closed := bc.ok("POST", "/api/v1/statuses", url.Values{"status": {"mine"}, "quote_approval_policy": {"nobody"}}).(map[string]any)
	if a := approval(ec, closed["id"].(string)); a["current_user"] != "denied" || len(a["automatic"].([]any)) != 0 {
		t.Fatalf("nobody: %v", a)
	}
	if a := approval(bc, closed["id"].(string)); a["current_user"] != "automatic" {
		t.Fatalf("author: %v", a)
	}
	if code, _, _ := ec.do("POST", "/api/v1/statuses", url.Values{"status": {"nope"}, "quoted_status_id": {closed["id"].(string)}}); code != 422 {
		t.Fatalf("got %d, want 422", code)
	}
	if code, _, _ := bc.do("POST", "/api/v1/statuses", url.Values{"status": {"x"}, "quote_approval_policy": {"everyone"}}); code != 422 {
		t.Fatalf("got %d, want 422", code)
	}

	followers := bc.ok("PUT", "/api/v1/statuses/"+closed["id"].(string)+"/interaction_policy", url.Values{"quote_approval_policy": {"followers"}}).(map[string]any)
	if a := followers["quote_approval"].(map[string]any); a["automatic"].([]any)[0] != "followers" {
		t.Fatalf("followers: %v", a)
	}
	if a := approval(ec, closed["id"].(string)); a["current_user"] != "denied" {
		t.Fatalf("non-follower: %v", a)
	}
	la.db.Exec(`INSERT INTO follows (id,uri,account_id,target_id,accepted,created_at) VALUES ('f3','x',?,?,1,0)`, eve.ID, bob.ID)
	if a := approval(ec, closed["id"].(string)); a["current_user"] != "automatic" {
		t.Fatalf("follower: %v", a)
	}
	if code, _, _ := ec.do("PUT", "/api/v1/statuses/"+closed["id"].(string)+"/interaction_policy", url.Values{"quote_approval_policy": {"public"}}); code != 404 {
		t.Fatalf("got %d, want 404", code)
	}

	q, err := la.PostStatus(ctx, eve, PostInput{Text: "quoting", Visibility: "public", QuoteOf: open["id"].(string)})
	if err != nil {
		t.Fatal(err)
	}
	if q.QuoteAuth == "" {
		t.Fatal("local quote of another user's post has no stamp")
	}
	revoked := bc.ok("POST", "/api/v1/statuses/"+open["id"].(string)+"/quotes/"+q.ID+"/revoke", nil).(map[string]any)
	if revoked["quote"].(map[string]any)["state"] != "revoked" {
		t.Fatalf("revoked = %v", revoked["quote"])
	}
	if got := bc.ok("GET", "/api/v1/statuses/"+open["id"].(string)+"/quotes", nil).([]any); len(got) != 0 {
		t.Fatalf("revoked quote still listed")
	}
	if code, _, _ := ec.do("POST", "/api/v1/statuses/"+open["id"].(string)+"/quotes/"+q.ID+"/revoke", nil); code != 404 {
		t.Fatalf("got %d, want 404", code)
	}
}

// Featured accounts (Mastodon's endorsements): pin/endorse, the
// relationship flag and both listings.
func TestAPIEndorsements(t *testing.T) {
	la, _, bob, alice := featureFixture(t)
	eve, _ := la.CreateLocalUser("eve", "password123", false)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write follow")}
	rel := c.ok("POST", "/api/v1/accounts/"+alice.ID+"/endorse", nil).(map[string]any)
	if rel["endorsed"] != true {
		t.Fatalf("relationship = %v", rel)
	}
	c.ok("POST", "/api/v1/accounts/"+eve.ID+"/pin", nil)
	ids := func(v any) (out []string) {
		for _, a := range v.([]any) {
			out = append(out, a.(map[string]any)["id"].(string))
		}
		return
	}
	if got := ids(c.ok("GET", "/api/v1/endorsements", nil)); len(got) != 2 || got[0] != eve.ID || got[1] != alice.ID {
		t.Fatalf("endorsements = %v", got)
	}
	if got := ids((&apiClient{t: t, base: la.base}).ok("GET", "/api/v1/accounts/"+bob.ID+"/endorsements", nil)); len(got) != 2 {
		t.Fatalf("public endorsements = %v", got)
	}
	if code, _, _ := c.do("POST", "/api/v1/accounts/"+bob.ID+"/endorse", nil); code != 422 {
		t.Fatalf("self-endorse: %d", code)
	}
	c.ok("POST", "/api/v1/accounts/"+alice.ID+"/unendorse", nil)
	if rels := c.ok("GET", "/api/v1/accounts/relationships?id[]="+alice.ID+"&id[]="+eve.ID, nil).([]any); rels[0].(map[string]any)["endorsed"] != false || rels[1].(map[string]any)["endorsed"] != true {
		t.Fatalf("relationships = %v", rels)
	}
	resp, err := http.Get(la.base + "/@bob")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if page := string(body); !strings.Contains(page, `class="featured"`) || !strings.Contains(page, eve.Path()) {
		t.Fatal("profile doesn't show featured accounts")
	}
}

// "blur" filters (Mastodon 4.4) keep the post but hide its media.
func TestAPIFilterBlur(t *testing.T) {
	la, _, bob, alice := featureFixture(t)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	st := &Status{ID: newID(), URI: alice.URI + "/s/blur", AccountID: alice.ID, Visibility: "public", Content: "<p>scary spider pics</p>", CreatedAt: time.Now()}
	la.insertStatus(st)
	f := c.ok("POST", "/api/v2/filters", map[string]any{"title": "spiders", "context": []string{"home"}, "filter_action": "blur",
		"keywords_attributes": []any{map[string]any{"keyword": "spider"}}}).(map[string]any)
	if f["filter_action"] != "blur" {
		t.Fatalf("filter_action = %v", f["filter_action"])
	}
	var got map[string]any
	for _, s := range c.ok("GET", "/api/v1/timelines/home", nil).([]any) {
		if s.(map[string]any)["id"] == st.ID {
			got = s.(map[string]any)
		}
	}
	if got == nil {
		t.Fatal("blurred post missing")
	}
	if fr := got["filtered"].([]any); len(fr) != 1 || fr[0].(map[string]any)["filter"].(map[string]any)["filter_action"] != "blur" {
		t.Fatalf("filtered = %v", got["filtered"])
	}
	sts, _ := la.homeTimeline(bob, "")
	for _, s := range la.applyFilters(bob, sts, "home") {
		if s.ID == st.ID && (!s.Blurred || s.FilteredBy != "") {
			t.Fatalf("web: blurred=%v filteredBy=%q", s.Blurred, s.FilteredBy)
		}
	}
}

// Deleting a post keeps its uploads for "delete and redraft" unless
// delete_media is set; unattached uploads can be deleted (Mastodon 4.4).
func TestAPIDeleteMedia(t *testing.T) {
	la, _, _, _ := featureFixture(t)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	upload := func() string {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "x.png")
		fw.Write(pngBytes())
		mw.Close()
		req, _ := http.NewRequest("POST", la.base+"/api/v2/media", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return m["id"].(string)
	}
	m1 := upload()
	st := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"pic"}, "media_ids[]": {m1}}).(map[string]any)
	if code, _, _ := c.do("DELETE", "/api/v1/media/"+m1, nil); code != 422 {
		t.Fatalf("deleted attached media: %d", code)
	}
	c.ok("DELETE", "/api/v1/statuses/"+st["id"].(string), nil)
	redraft := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"pic again"}, "media_ids[]": {m1}}).(map[string]any)
	if len(redraft["media_attachments"].([]any)) != 1 {
		t.Fatal("redraft lost its media")
	}
	c.ok("DELETE", "/api/v1/statuses/"+redraft["id"].(string)+"?delete_media=true", nil)
	if n := la.countInt(`SELECT COUNT(*) FROM attachments WHERE id=?`, m1); n != 0 {
		t.Fatal("delete_media kept the upload")
	}
	m2 := upload()
	c.ok("DELETE", "/api/v1/media/"+m2, nil)
	if code, _, _ := c.do("GET", "/api/v1/media/"+m2, nil); code != 404 {
		t.Fatalf("deleted media still there: %d", code)
	}
}

// attribution_domains (Mastodon 4.4): set through the API, shown in
// source and published on the actor; remote ones are read.
func TestAttributionDomains(t *testing.T) {
	la, _, bob, _ := featureFixture(t)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	me := c.ok("PATCH", "/api/v1/accounts/update_credentials", url.Values{"attribution_domains[]": {"https://Blog.Example/", "news.example", "blog.example"}}).(map[string]any)
	got := me["source"].(map[string]any)["attribution_domains"].([]any)
	if len(got) != 2 || got[0] != "blog.example" || got[1] != "news.example" {
		t.Fatalf("attribution_domains = %v", got)
	}
	if code, _, _ := c.do("PATCH", "/api/v1/accounts/update_credentials", url.Values{"attribution_domains[]": {"not a domain"}}); code != 422 {
		t.Fatalf("invalid domain accepted: %d", code)
	}
	acc, _ := la.AccountByID(bob.ID)
	if d := la.actorJSON(acc)["attributionDomains"]; !reflect.DeepEqual(d, []string{"blog.example", "news.example"}) {
		t.Fatalf("actor attributionDomains = %v", d)
	}
	if list, err := attributionDomains([]string{"xn--bcher-kva.example", "a.example/path?x"}); err != nil || list[1] != "a.example" {
		t.Fatalf("%v %v", list, err)
	}
}

// The default quote policy (Mastodon 4.5 source[quote_policy]) applies to
// new posts and shows up in preferences.
func TestDefaultQuotePolicy(t *testing.T) {
	la, _, _, _ := featureFixture(t)
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	me := c.ok("PATCH", "/api/v1/accounts/update_credentials", url.Values{"source[quote_policy]": {"followers"}}).(map[string]any)
	if me["source"].(map[string]any)["quote_policy"] != "followers" {
		t.Fatalf("source = %v", me["source"])
	}
	if p := c.ok("GET", "/api/v1/preferences", nil).(map[string]any); p["posting:default:quoted_policy"] != "followers" {
		t.Fatalf("preferences = %v", p)
	}
	st := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"hi"}}).(map[string]any)
	if a := st["quote_approval"].(map[string]any)["automatic"].([]any); len(a) != 1 || a[0] != "followers" {
		t.Fatalf("new post quote_approval = %v", st["quote_approval"])
	}
	st = c.ok("POST", "/api/v1/statuses", url.Values{"status": {"hi"}, "quote_approval_policy": {"public"}}).(map[string]any)
	if a := st["quote_approval"].(map[string]any)["automatic"].([]any); a[0] != "public" {
		t.Fatalf("explicit policy = %v", st["quote_approval"])
	}
	if code, _, _ := c.do("PATCH", "/api/v1/accounts/update_credentials", url.Values{"source[quote_policy]": {"everyone"}}); code != 422 {
		t.Fatalf("invalid policy: %d", code)
	}
}

// Several accounts or posts at once (Mastodon 4.3), without the hidden ones.
func TestAPIMultiFetch(t *testing.T) {
	la, _, bob, alice := featureFixture(t)
	ctx := context.Background()
	eve, _ := la.CreateLocalUser("eve", "password123", false)
	pub, _ := la.PostStatus(ctx, eve, PostInput{Text: "public", Visibility: "public"})
	priv, _ := la.PostStatus(ctx, eve, PostInput{Text: "secret", Visibility: "private"})
	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read")}
	accs := c.ok("GET", "/api/v1/accounts?id[]="+alice.ID+"&id[]=nope&id[]="+bob.ID, nil).([]any)
	if len(accs) != 2 || accs[0].(map[string]any)["id"] != alice.ID {
		t.Fatalf("accounts = %v", accs)
	}
	sts := c.ok("GET", "/api/v1/statuses?id[]="+pub.ID+"&id[]="+priv.ID, nil).([]any)
	if len(sts) != 1 || sts[0].(map[string]any)["id"] != pub.ID {
		t.Fatalf("statuses = %v", sts)
	}
}
