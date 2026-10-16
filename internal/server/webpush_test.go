package server

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestBrowserNotifications(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	var mu sync.Mutex
	var got [][]byte
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		got = append(got, b)
		mu.Unlock()
		w.WriteHeader(201)
	}))
	defer svc.Close()

	c := webClient(t, la, "bob", "password123")
	page, _ := c.Get(la.base + "/settings")
	b, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(b), `data-key="`+la.vapidPublic()+`"`) {
		t.Fatal("settings page lacks the VAPID key")
	}
	csrf := strings.SplitN(strings.SplitN(string(b), `name="csrf" value="`, 2)[1], `"`, 2)[0]
	post := func(form url.Values) int {
		form.Set("csrf", csrf)
		req, _ := http.NewRequest("POST", la.base+"/settings/webpush", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(url.Values{"endpoint": {"ftp://nope"}, "p256dh": {"x"}, "auth": {"y"}}); code != 400 {
		t.Fatalf("bad endpoint: %d", code)
	}
	if code := post(url.Values{"endpoint": {svc.URL + "/push/web"}, "p256dh": {b64url.EncodeToString(ua.PublicKey().Bytes())}, "auth": {b64url.EncodeToString(auth)}}); code != 204 {
		t.Fatalf("subscribe: %d", code)
	}
	mine, _ := la.PostStatus(t.Context(), bob, PostInput{Text: "push me in the browser", Visibility: "public"})
	r.send(obj{"@context": asContext, "id": r.Base + "/likes/webpush", "type": "Like", "actor": alice.URI, "object": mine.URI}, r.Base+"/users/alice#main-key", sigCavage)
	eventually(t, "browser push delivered", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 })
	var payload map[string]any
	if err := json.Unmarshal(decryptPush(t, got[0], ua, auth), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["url"] != mine.Path() || !strings.Contains(payload["title"].(string), "favourited") {
		t.Fatalf("payload = %v", payload)
	}
	post(url.Values{"action": {"unsubscribe"}, "endpoint": {svc.URL + "/push/web"}})
	if n := la.countInt(`SELECT COUNT(*) FROM web_push_subscriptions`); n != 0 {
		t.Fatalf("%d subscriptions left", n)
	}
	resp, err := http.Get(la.base + "/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	sw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") || !strings.Contains(string(sw), "showNotification") {
		t.Fatal("service worker not served")
	}
}
