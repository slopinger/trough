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

func TestFocalPoints(t *testing.T) {
	la, r, _, _ := featureFixture(t)
	token := oauthLogin(t, la, "bob", "password123", "read write")
	c := &apiClient{t: t, base: la.base, token: token}
	upload := func(fields map[string]string) map[string]any {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", "pic.png")
		fw.Write(pngBytes())
		for k, v := range fields {
			mw.WriteField(k, v)
		}
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
		return m
	}
	focus := func(m map[string]any) any { meta, _ := m["meta"].(map[string]any); return meta["focus"] }

	if f := focus(upload(nil)); f != nil {
		t.Errorf("focus without one set = %v", f)
	}
	media := upload(map[string]string{"focus": "0.5,-0.25"})
	if f, _ := focus(media).(map[string]any); f["x"] != 0.5 || f["y"] != -0.25 {
		t.Fatalf("uploaded focus = %v", focus(media))
	}
	// updating clamps to -1..1; nonsense is ignored
	id := media["id"].(string)
	if f, _ := focus(c.ok("PUT", "/api/v1/media/"+id, url.Values{"focus": {"2,0.3"}}).(map[string]any)).(map[string]any); f["x"] != 1.0 || f["y"] != 0.3 {
		t.Fatalf("clamped focus = %v", f)
	}
	if f, _ := focus(c.ok("PUT", "/api/v1/media/"+id, url.Values{"focus": {"NaN,1"}}).(map[string]any)).(map[string]any); f["x"] != 1.0 {
		t.Fatalf("NaN changed the focus: %v", f)
	}

	// The post federates it as focalPoint and the web UI hands it to app.js.
	st := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"look"}, "media_ids[]": {id}, "visibility": {"public"}}).(map[string]any)
	if f, _ := focus(st["media_attachments"].([]any)[0].(map[string]any)).(map[string]any); f["x"] != 1.0 {
		t.Errorf("status media focus = %v", f)
	}
	req, _ := http.NewRequest("GET", st["uri"].(string), nil)
	req.Header.Set("Accept", "application/activity+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	note, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(note), `"focalPoint":[1,0.3]`) {
		t.Errorf("note lacks focalPoint: %s", note)
	}
	resp, err = http.Get(la.base + "/s/" + st["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), `data-focus="1.00,0.30"`) {
		t.Error("web page lacks data-focus")
	}

	// Incoming focal points are kept, clamped.
	r.put("/users/alice/statuses/f1", `{"id": "{{R}}/users/alice/statuses/f1", "type": "Note", "attributedTo": "{{R}}/users/alice",
		"content": "<p>pic</p>", "to": ["https://www.w3.org/ns/activitystreams#Public"], "published": "2026-01-01T00:00:00Z",
		"attachment": [{"type": "Document", "mediaType": "image/jpeg", "url": "{{R}}/pic.jpg", "focalPoint": [-0.5, 7]}]}`)
	remote, err := la.resolveStatus(context.Background(), r.Base+"/users/alice/statuses/f1")
	if err != nil {
		t.Fatal(err)
	}
	got := c.ok("GET", "/api/v1/statuses/"+remote.ID, nil).(map[string]any)
	if f, _ := focus(got["media_attachments"].([]any)[0].(map[string]any)).(map[string]any); f["x"] != -0.5 || f["y"] != 1.0 {
		t.Errorf("incoming focus = %v", f)
	}
}

func TestEditMediaAttributes(t *testing.T) {
	la, _, _, _ := featureFixture(t)
	token := oauthLogin(t, la, "bob", "password123", "read write")
	c := &apiClient{t: t, base: la.base, token: token}
	upload := func() string {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", "pic.png")
		fw.Write(pngBytes())
		mw.WriteField("description", "old words")
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
	a, b, stray := upload(), upload(), upload()
	st := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"two pics"}, "media_ids[]": {a, b}, "visibility": {"public"}}).(map[string]any)
	path := "/api/v1/statuses/" + st["id"].(string)
	media := func(v any) []any { return v.(map[string]any)["media_attachments"].([]any) }

	// JSON, as Mastodon's web UI sends it; an id that isn't part of the post is ignored
	got := media(c.ok("PUT", path, map[string]any{"status": "two pics, edited", "media_attributes": []any{
		map[string]any{"id": a, "description": "a pig in a field", "focus": "0.0,-0.8"},
		map[string]any{"id": stray, "description": "not mine to change"},
	}}))
	first := got[0].(map[string]any)
	if first["description"] != "a pig in a field" || first["meta"].(map[string]any)["focus"].(map[string]any)["y"] != -0.8 {
		t.Errorf("JSON edit: %v", first)
	}
	if got[1].(map[string]any)["description"] != "old words" {
		t.Errorf("untouched attachment changed: %v", got[1])
	}
	var strayDesc string
	la.db.QueryRow(`SELECT description FROM attachments WHERE id=?`, stray).Scan(&strayDesc)
	if strayDesc != "old words" {
		t.Error("an attachment outside the post was changed")
	}
	// Rails-style form lists
	got = media(c.ok("PUT", path, url.Values{"status": {"again"}, "media_attributes[][id]": {a, b}, "media_attributes[][description]": {"pig", "checkerboard"}}))
	if got[0].(map[string]any)["description"] != "pig" || got[1].(map[string]any)["description"] != "checkerboard" {
		t.Errorf("form edit: %v", got)
	}
}

func TestWebEditMedia(t *testing.T) {
	la, _, _, _ := featureFixture(t)
	token := oauthLogin(t, la, "bob", "password123", "read write")
	c := &apiClient{t: t, base: la.base, token: token}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "pic.png")
	fw.Write(pngBytes())
	mw.WriteField("description", "old words")
	mw.Close()
	req, _ := http.NewRequest("POST", la.base+"/api/v2/media", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var media map[string]any
	json.NewDecoder(resp.Body).Decode(&media)
	resp.Body.Close()
	id := media["id"].(string)
	st := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"a pic"}, "media_ids[]": {id}, "visibility": {"public"}}).(map[string]any)
	sid := st["id"].(string)

	web := webClient(t, la, "bob", "password123")
	resp, err = web.Get(la.base + "/compose?edit=" + sid)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(b)
	if !strings.Contains(page, `name="edit_alt_`+id+`"`) || !strings.Contains(page, ">old words</textarea>") {
		t.Fatal("edit form doesn't offer the attachment's alt text")
	}
	csrf := strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0]
	form := url.Values{"csrf": {csrf}, "text": {"a pic, edited"}, "edit_alt_" + id: {"a pig in a field"}, "edit_focus_" + id: {"0.5,-0.5"}}
	req, _ = http.NewRequest("POST", la.base+"/statuses/"+sid+"/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", la.base)
	if resp, err = web.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := c.ok("GET", "/api/v1/statuses/"+sid, nil).(map[string]any)
	m := got["media_attachments"].([]any)[0].(map[string]any)
	if m["description"] != "a pig in a field" || m["meta"].(map[string]any)["focus"].(map[string]any)["x"] != 0.5 || got["edited_at"] == nil {
		t.Errorf("after web edit: %v", m)
	}
}
