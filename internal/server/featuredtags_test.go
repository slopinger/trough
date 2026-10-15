package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestFeaturedTags(t *testing.T) {
	la := startLiveApp(t)
	ann, _ := la.CreateLocalUser("ann", "password123", false)
	ctx := context.Background()
	la.PostStatus(ctx, ann, PostInput{Text: "my #Cats", Visibility: "public"})
	la.PostStatus(ctx, ann, PostInput{Text: "more #cats", Visibility: "public"})
	la.PostStatus(ctx, ann, PostInput{Text: "secret #cats", Visibility: "direct"})
	la.PostStatus(ctx, ann, PostInput{Text: "#dogs too", Visibility: "public"})

	c := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "ann", "password123", "read write")}
	sugg := c.ok("GET", "/api/v1/featured_tags/suggestions", nil).([]any)
	if len(sugg) != 2 || sugg[0].(map[string]any)["name"] != "cats" {
		t.Fatalf("suggestions = %v", sugg)
	}
	f := c.ok("POST", "/api/v1/featured_tags", map[string]any{"name": "#Cats"}).(map[string]any)
	if f["name"] != "cats" || f["statuses_count"] != "2" || len(f["last_status_at"].(string)) != 10 ||
		f["url"] != la.base+"/@ann/tagged/cats" {
		t.Fatalf("featured tag = %v", f)
	}
	if tag := c.ok("POST", "/api/v1/tags/dogs/feature", nil).(map[string]any); tag["featuring"] != true {
		t.Fatalf("tag = %v", tag)
	}
	public := (&apiClient{t: t, base: la.base}).ok("GET", "/api/v1/accounts/"+ann.ID+"/featured_tags", nil).([]any)
	if len(public) != 2 {
		t.Fatalf("account featured tags = %v", public)
	}
	c.ok("DELETE", "/api/v1/featured_tags/"+f["id"].(string), nil)
	if got := c.ok("GET", "/api/v1/featured_tags", nil).([]any); len(got) != 1 || got[0].(map[string]any)["name"] != "dogs" {
		t.Fatalf("after delete = %v", got)
	}

	actor := la.actorJSON(ann)
	if actor["featuredTags"] != ann.URI+"/collections/tags" {
		t.Fatalf("actor featuredTags = %v", actor["featuredTags"])
	}
	req, _ := http.NewRequest("GET", ann.URI+"/collections/tags", nil)
	req.Header.Set("Accept", "application/activity+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"name":"#dogs"`) || !strings.Contains(string(body), `"type":"Hashtag"`) {
		t.Fatalf("collection = %s", body)
	}
	for path, want := range map[string]string{"/@ann": "/@ann/tagged/dogs", "/@ann/tagged/cats": "more"} {
		resp, err := http.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), want) {
			t.Fatalf("%s: %d, missing %q", path, resp.StatusCode, want)
		}
	}
}
