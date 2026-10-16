package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAltBadge(t *testing.T) {
	la := startLiveApp(t)
	la.CreateLocalUser("bob", "password123", false)
	token := oauthLogin(t, la, "bob", "password123", "read write")
	c := &apiClient{t: t, base: la.base, token: token}
	upload := func(desc string) string {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", "pic.png")
		fw.Write(pngBytes())
		mw.WriteField("description", desc)
		mw.Close()
		req, _ := http.NewRequest("POST", la.base+"/api/v2/media", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return m["id"].(string)
	}
	st := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"pics"}, "visibility": {"public"},
		"media_ids[]": {upload("a pig <in> a field"), upload("")}}).(map[string]any)
	resp, err := http.Get(la.base + "/s/" + st["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(b)
	if strings.Count(page, `class="alt-badge"`) != 1 || !strings.Contains(page, "<summary title=\"Show the description\">ALT</summary><p>a pig &lt;in&gt; a field</p>") {
		t.Error("described picture lacks its ALT badge, or the other one has one")
	}
}
