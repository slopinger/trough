package server

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
)

func TestAutocomplete(t *testing.T) {
	la, _, bob, alice := featureFixture(t)
	la.CreateLocalUser("alfred", "password123", false)
	la.PostStatus(context.Background(), bob, PostInput{Text: "#trough #troughs #trough", Visibility: "public"})
	la.db.Exec(`INSERT INTO custom_emojis (shortcode, file, url, media_type, created_at) VALUES ('blobcat', 'x.png', '/media/x.png', 'image/png', 0)`)
	c := webClient(t, la, "bob", "password123")
	get := func(q string) []suggestion {
		resp, err := c.Get(la.base + "/autocomplete?q=" + url.QueryEscape(q))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out []suggestion
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return out
	}
	// alice is followed, so she comes before alfred
	if got := get("@al"); len(got) != 2 || got[0].Insert != "@"+alice.Acct() || got[1].Insert != "@alfred" {
		t.Fatalf("@al = %+v", got)
	}
	if got := get("#tro"); len(got) != 2 || got[0].Insert != "#trough" {
		t.Fatalf("#tro = %+v", got)
	}
	if got := get(":blo"); len(got) != 1 || got[0].Insert != ":blobcat:" {
		t.Fatalf(":blo = %+v", got)
	}
	if got := get("@a%"); len(got) != 0 {
		t.Fatalf("LIKE wildcards not escaped: %+v", got)
	}
}
