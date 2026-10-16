package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestPreferences(t *testing.T) {
	la := startLiveApp(t)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	cat, _ := la.CreateLocalUser("cat", "password123", false)
	ctx := context.Background()
	token := oauthLogin(t, la, "bob", "password123", "read write")
	c := &apiClient{t: t, base: la.base, token: token}

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
	page := get("/settings")
	if !strings.Contains(page, `id="preferences"`) {
		t.Fatal("settings have no preferences")
	}
	csrf := strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0]
	form := url.Values{"csrf": {csrf}, "visibility": {"unlisted"}, "language": {"de"}, "sensitive": {"on"}, "media": {"hide_all"}, "spoilers": {"on"}}
	req, _ := http.NewRequest("POST", la.base+"/settings/preferences", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", la.base)
	if resp, err := web.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}

	// The compose box starts from them; replies stay as private as their parent.
	home := get("/")
	if !strings.Contains(home, `value="unlisted" selected`) || !strings.Contains(home, `value="de" selected`) ||
		!strings.Contains(home, `name="sensitive" checked`) {
		t.Error("home compose ignores the defaults")
	}
	secret, _ := la.PostStatus(ctx, cat, PostInput{Text: "@bob followers only", Visibility: "private"})
	if p := get(secret.Path()); !strings.Contains(p, `value="private" selected`) {
		t.Error("reply to a followers-only post isn't followers-only")
	}

	// Clients get them through the API, and posts without the params use them.
	prefs := c.ok("GET", "/api/v1/preferences", nil).(map[string]any)
	if prefs["posting:default:visibility"] != "unlisted" || prefs["posting:default:language"] != "de" ||
		prefs["reading:expand:media"] != "hide_all" || prefs["reading:expand:spoilers"] != true {
		t.Errorf("preferences = %v", prefs)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "pic.png")
	fw.Write(pngBytes())
	mw.Close()
	up, _ := http.NewRequest("POST", la.base+"/api/v2/media", &body)
	up.Header.Set("Content-Type", mw.FormDataContentType())
	up.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(up)
	if err != nil {
		t.Fatal(err)
	}
	var media map[string]any
	json.NewDecoder(resp.Body).Decode(&media)
	resp.Body.Close()
	st := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"ein Bild"}, "media_ids[]": {media["id"].(string)}}).(map[string]any)
	if st["visibility"] != "unlisted" || st["language"] != "de" || st["sensitive"] != true {
		t.Errorf("status without params: %v %v %v", st["visibility"], st["language"], st["sensitive"])
	}
	me := c.ok("PATCH", "/api/v1/accounts/update_credentials", url.Values{"source[privacy]": {"private"}, "source[sensitive]": {"false"}}).(map[string]any)
	if src := me["source"].(map[string]any); src["privacy"] != "private" || src["sensitive"] != false || src["language"] != "de" {
		t.Errorf("source = %v", src)
	}
	if code, _, _ := c.do("PATCH", "/api/v1/accounts/update_credentials", url.Values{"source[privacy]": {"secret"}}); code != 422 {
		t.Errorf("invalid privacy: %d", code)
	}

	// Reading: content warnings open, all media behind a click.
	la.PostStatus(ctx, cat, PostInput{Text: "the reveal", Spoiler: "pig spoilers", Visibility: "public"})
	la.db.Exec(`UPDATE statuses SET sensitive=0 WHERE id=?`, st["id"])
	if p := get("/@bob"); !strings.Contains(p, "Media hidden — click to show") {
		t.Error("hide_all didn't hide unmarked media")
	}
	if p := get("/@cat"); !strings.Contains(p, `<details class="cw" open>`) {
		t.Error("content warning not expanded")
	}
	la.db.Exec(`UPDATE statuses SET sensitive=1 WHERE id=?`, st["id"])
	la.db.Exec(`UPDATE users SET expand_media='show_all', expand_spoilers=0 WHERE account_id=?`, bob.ID)
	if p := get("/@bob"); strings.Contains(p, `class="sensitive"`) {
		t.Error("show_all still hides sensitive media")
	}
	if p := get("/@cat"); strings.Contains(p, `<details class="cw" open>`) {
		t.Error("content warning expanded after turning it off")
	}
}
