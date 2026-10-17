package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestChosenLanguages(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	carol, _ := la.CreateLocalUser("carol", "password123", false)
	en, _ := la.PostStatus(ctx, carol, PostInput{Text: "hello #bread", Visibility: "public", Language: "en"})
	de, _ := la.PostStatus(ctx, carol, PostInput{Text: "hallo #bread", Visibility: "public", Language: "de"})
	ptBR, _ := la.PostStatus(ctx, carol, PostInput{Text: "olá #bread", Visibility: "public", Language: "pt-BR"})
	none, _ := la.PostStatus(ctx, carol, PostInput{Text: "🍞 #bread", Visibility: "public"})
	la.db.Exec(`UPDATE statuses SET language='' WHERE id=?`, none.ID) // no language at all

	// bob picks English and Portuguese in the settings form
	web := webClient(t, la, "bob", "password123")
	resp, _ := web.Get(la.base + "/settings")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), `name="languages" value="de"`) {
		t.Fatal("settings lack the language choice")
	}
	csrf := strings.SplitN(strings.SplitN(string(page), `name="csrf" value="`, 2)[1], `"`, 2)[0]
	form := url.Values{"csrf": {csrf}, "visibility": {"public"}, "media": {"default"}, "languages": {"pt", "en", "xx"}}
	req, _ := http.NewRequest("POST", la.base+"/settings/preferences", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", la.base)
	if resp, err := web.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	if got := la.prefs(bob.ID).Languages; strings.Join(got, ",") != "en,pt" {
		t.Fatalf("stored languages %v", got)
	}

	ids := func(sts []*Status) map[string]bool {
		out := map[string]bool{}
		for _, s := range sts {
			out[s.ID] = true
		}
		return out
	}
	for name, get := range map[string]func(*Account) ([]*Status, error){
		"local":     func(v *Account) ([]*Status, error) { return la.localTimeline(v, "") },
		"federated": func(v *Account) ([]*Status, error) { return la.federatedTimeline(v, "") },
		"tag":       func(v *Account) ([]*Status, error) { return la.tagTimeline(v, "bread", "") },
	} {
		sts, _ := get(bob)
		got := ids(sts)
		if !got[en.ID] || got[de.ID] || !got[ptBR.ID] || !got[none.ID] {
			t.Errorf("%s for bob: en %v de %v pt-BR %v none %v", name, got[en.ID], got[de.ID], got[ptBR.ID], got[none.ID])
		}
		if sts, _ := get(carol); len(sts) != 4 {
			t.Errorf("%s for carol, who chose nothing: %d posts", name, len(sts))
		}
	}
	// an app updating other preferences keeps the choice
	api := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read write")}
	api.ok("PATCH", "/api/v1/accounts/update_credentials", url.Values{"source[privacy]": {"unlisted"}})
	if got := la.prefs(bob.ID); got.Visibility != "unlisted" || len(got.Languages) != 2 {
		t.Errorf("after the api update: %+v", got)
	}
}
