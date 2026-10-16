package server

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"
)

// The web UI keeps unsent drafts in the browser, keyed by the compose form's data-draft.
func TestComposeDraftKeys(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	st, _ := la.PostStatus(context.Background(), bob, PostInput{Text: "hello", Visibility: "public"})
	web := webClient(t, la, "bob", "password123")
	get := func(path string) string {
		t.Helper()
		resp, err := web.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	for path, key := range map[string]string{
		"/compose":                   bob.ID + ":new",
		"/compose?reply_to=" + st.ID: bob.ID + ":reply:" + st.ID,
		"/compose?quote=" + st.ID:    bob.ID + ":quote:" + st.ID,
	} {
		if page := get(path); !strings.Contains(page, `data-draft="`+key+`"`) || !strings.Contains(page, `class="draft-status`) {
			t.Errorf("%s: no draft key %q", path, key)
		}
	}
	if page := get("/compose?edit=" + st.ID); !strings.Contains(page, "Editing your post") || strings.Contains(page, "data-draft=") {
		t.Error("editing a post shouldn't keep a draft")
	}
}

func TestComposeLanguage(t *testing.T) {
	la := startLiveApp(t)
	la.CreateLocalUser("bob", "password123", false)
	web := webClient(t, la, "bob", "password123")
	page := func() string {
		t.Helper()
		resp, err := web.Get(la.base + "/compose")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	first := page()
	if !strings.Contains(first, `<option value="" selected>Language…</option>`) || !strings.Contains(first, `<option value="de">Deutsch</option>`) {
		t.Fatal("compose form has no unset language selector")
	}
	csrf := strings.SplitN(strings.SplitN(first, `name="csrf" value="`, 2)[1], `"`, 2)[0]
	resp, err := web.PostForm(la.base+"/statuses", url.Values{"csrf": {csrf}, "text": {"hallo zusammen"}, "visibility": {"public"}, "language": {"de"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var lang string
	la.db.QueryRow(`SELECT language FROM statuses WHERE content LIKE '%hallo zusammen%'`).Scan(&lang)
	if lang != "de" {
		t.Fatalf("post language = %q", lang)
	}
	if !strings.Contains(page(), `<option value="de" selected>Deutsch</option>`) {
		t.Error("the next post doesn't start with the last language")
	}
}
