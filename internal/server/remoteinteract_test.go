package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRemoteInteract(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	withTemplate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/webfinger" || r.URL.Query().Get("resource") != "acct:me@"+r.Host {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/jrd+json")
		w.Write([]byte(`{"links":[{"rel":"http://ostatus.org/schema/1.0/subscribe","template":"http://` + r.Host + `/follow?acct={uri}"}]}`))
	}))
	defer withTemplate.Close()
	without := httptest.NewServer(http.NotFoundHandler())
	defer without.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	interact := func(handle string) string {
		form := url.Values{"uri": {bob.URI}, "back": {"/@bob"}, "handle": {handle}}
		req, _ := http.NewRequest("POST", la.base+"/interact", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	h1 := strings.TrimPrefix(withTemplate.URL, "http://")
	if got := interact("@me@" + h1); got != "http://"+h1+"/follow?acct="+url.QueryEscape(bob.URI) {
		t.Fatalf("with template: %s", got)
	}
	h2 := strings.TrimPrefix(without.URL, "http://")
	if got := interact("me@" + h2); got != "http://"+h2+"/authorize_interaction?uri="+url.QueryEscape(bob.URI) {
		t.Fatalf("fallback: %s", got)
	}
	if got := interact("just-a-name"); got != "/@bob" {
		t.Fatalf("bad handle: %s", got)
	}
	resp, _ := http.Get(la.base + "/interact?uri=" + url.QueryEscape("https://elsewhere.example/x"))
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("foreign uri: %d", resp.StatusCode)
	}
}
