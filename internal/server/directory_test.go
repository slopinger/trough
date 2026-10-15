package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDirectory(t *testing.T) {
	la, r, _, _ := featureFixture(t)
	ctx := context.Background()
	ann, _ := la.CreateLocalUser("ann", "password123", false)
	hidden, _ := la.CreateLocalUser("hidden", "password123", false)
	hidden.Discoverable = false
	la.saveAccount(hidden)
	la.PostStatus(ctx, ann, PostInput{Text: "hi", Visibility: "public"})
	la.PostStatus(ctx, hidden, PostInput{Text: "hi", Visibility: "public"})
	r.put("/users/carol", strings.Replace(strings.ReplaceAll(mastodonActor, "/users/alice", "/users/carol"), `"preferredUsername": "alice"`, `"preferredUsername": "carol", "discoverable": true`, 1))
	carol, err := la.getAccount(ctx, r.Base+"/users/carol")
	if err != nil || !carol.Discoverable {
		t.Fatalf("remote discoverable flag: %v %v", err, carol)
	}

	c := &apiClient{t: t, base: la.base}
	names := func(path string) (out []string) {
		for _, a := range c.ok("GET", path, nil).([]any) {
			out = append(out, a.(map[string]any)["acct"].(string))
		}
		return
	}
	if got := names("/api/v1/directory?local=true"); len(got) != 1 || got[0] != "ann" {
		t.Fatalf("local active directory = %v", got)
	}
	// newest first, posts not needed; hidden opted out, the remote actors say discoverable: true
	if got := strings.Join(names("/api/v1/directory?order=new"), " "); !strings.HasPrefix(got, carol.Acct()+" ") ||
		strings.Count(got, " ") != 3 || strings.Contains(got, "hidden") {
		t.Fatalf("new directory = %v", got)
	}
	resp, err := http.Get(la.base + "/directory?order=new&scope=all")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "@ann") || strings.Contains(string(body), "@hidden") {
		t.Fatal("directory page lists the wrong people")
	}
}
