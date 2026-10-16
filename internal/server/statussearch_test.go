package server

import (
	"context"
	"io"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseStatusQuery(t *testing.T) {
	sq := parseStatusQuery(`cats "black cat" -dogs -"wet dog" from:me -from:bob@x.example HAS:media -is:reply in:library url:foo before:2026-01-02`)
	if want := []string{"cats", "black cat", "url:foo"}; !reflect.DeepEqual(sq.Terms, want) {
		t.Errorf("terms = %q, want %q", sq.Terms, want)
	}
	if want := []string{"dogs", "wet dog"}; !reflect.DeepEqual(sq.Exclude, want) {
		t.Errorf("exclude = %q, want %q", sq.Exclude, want)
	}
	want := []searchOp{{"from", "me", false}, {"from", "bob@x.example", true}, {"has", "media", false}, {"is", "reply", true}, {"before", "2026-01-02", false}}
	if !reflect.DeepEqual(sq.Ops, want) || !sq.Library {
		t.Errorf("ops = %+v library=%v", sq.Ops, sq.Library)
	}
	if plain := parseStatusQuery("just words"); plain.HasOperators() || plain.Text() != "just words" {
		t.Errorf("plain query = %+v", plain)
	}
	for _, q := range []string{"", `""`, "-", "in:all"} {
		if sq := parseStatusQuery(q); len(sq.Terms)+len(sq.Ops) != 0 {
			t.Errorf("%q parsed to %+v", q, sq)
		}
	}
}

func TestSearchOperators(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	ann, _ := la.CreateLocalUser("ann", "password123", false)
	post := func(a *Account, in PostInput) *Status {
		t.Helper()
		if in.Visibility == "" {
			in.Visibility = "public"
		}
		st, err := la.PostStatus(ctx, a, in)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	img, err := la.saveImage(bob, pngBytes(), "a sleepy kitten")
	if err != nil {
		t.Fatal(err)
	}
	old := post(ann, PostInput{Text: "ancient cats"})
	la.db.Exec(`UPDATE statuses SET created_at=? WHERE id=?`, time.Date(2020, 5, 17, 12, 0, 0, 0, time.UTC).Unix(), old.ID)
	withMedia := post(bob, PostInput{Text: "look at this", MediaIDs: []string{img.ID}})
	bobCats := post(bob, PostInput{Text: "cats are great", Language: "en"})
	annCats := post(ann, PostInput{Text: "cats and dogs", Language: "de"})
	reply := post(ann, PostInput{Text: "@bob cats agree", InReplyTo: bobCats.ID})
	cw := post(ann, PostInput{Text: "cats again", Spoiler: "cat pics"})
	poll := post(ann, PostInput{Text: "cats or not", Poll: &Poll{Options: []PollOption{{Name: "yes"}, {Name: "no"}}, EndTime: time.Now().Add(time.Hour)}})
	post(ann, PostInput{Text: "secret cats", Visibility: "private"})

	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	search := func(q string, extra ...string) []string {
		t.Helper()
		u := "/api/v2/search?type=statuses&q=" + url.QueryEscape(q)
		if len(extra) > 0 {
			u += "&" + extra[0]
		}
		var ids []string
		for _, s := range c.ok("GET", u, nil).(map[string]any)["statuses"].([]any) {
			ids = append(ids, s.(map[string]any)["id"].(string))
		}
		return ids
	}
	expect := func(q string, want ...*Status) {
		t.Helper()
		var wantIDs []string
		for i := len(want) - 1; i >= 0; i-- { // newest first
			wantIDs = append(wantIDs, want[i].ID)
		}
		if got := search(q); !reflect.DeepEqual(got, wantIDs) {
			t.Errorf("%q = %v, want %v", q, got, wantIDs)
		}
	}
	expect("cats", old, bobCats, annCats, reply, cw, poll)
	expect("cats -dogs", old, bobCats, reply, cw, poll)
	expect(`"cats and"`, annCats)
	expect("kitten", withMedia) // matches the image description
	expect("has:media", withMedia)
	expect("has:image", withMedia)
	expect("has:video")
	expect("cats has:poll", poll)
	expect("cats is:reply", reply)
	expect("cats -is:reply", old, bobCats, annCats, cw, poll)
	expect("pics is:sensitive", cw)
	expect("from:me", withMedia, bobCats)
	expect("cats from:ann -is:reply -has:poll", old, annCats, cw)
	expect("cats from:bob@"+la.cfg.Host, bobCats)
	expect("cats -from:ann", bobCats)
	expect("from:nobody cats")
	expect("cats language:de", annCats)
	expect("cats before:2021-01-01", old)
	expect("cats during:2020-05-17", old)
	expect("cats after:2020-05-17", bobCats, annCats, reply, cw, poll)
	expect("from:ann", old, annCats, reply, cw, poll) // not her followers-only post
	if got := search("from:ann", "account_id="+bob.ID); len(got) != 0 {
		t.Errorf("account_id filter = %v", got)
	}

	la.Like(bob, annCats)
	la.Bookmark(bob, cw)
	expect("in:library", withMedia, bobCats, annCats, reply, cw) // reply mentions bob
	expect("cats in:library -from:me", annCats, reply, cw)

	la.Follow(bob, ann)
	la.db.Exec(`UPDATE follows SET accepted=1 WHERE account_id=? AND target_id=?`, bob.ID, ann.ID)
	expect("secret") // followers-only posts aren't searched unless in your library
	if res := c.ok("GET", "/api/v2/search?q="+url.QueryEscape("from:me"), nil).(map[string]any); len(res["accounts"].([]any)) != 0 || len(res["hashtags"].([]any)) != 0 {
		t.Errorf("operator-only query matched accounts/tags: %v", res)
	}

	// web UI: operators applied and offered as links
	web := webClient(t, la, "bob", "password123")
	get := func(q string) string {
		t.Helper()
		resp, err := web.Get(la.base + "/search?q=" + url.QueryEscape(q))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	page := get("cats has:poll")
	if !strings.Contains(page, "cats or not") || strings.Contains(page, "cats are great") {
		t.Errorf("web search page doesn't apply operators:\n%s", page)
	}
	if !strings.Contains(page, `href="/search?q=cats%20has%3apoll%20from%3ame"`) {
		t.Error("operator links don't extend the current query")
	}
	if page := get("from:me -has:media"); !strings.Contains(page, "cats are great") || strings.Contains(page, "look at this") || strings.Contains(page, "No matching accounts") {
		t.Error("web from:me -has:media")
	}
}
