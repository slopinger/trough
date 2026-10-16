package server

import (
	"context"
	"io"
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
