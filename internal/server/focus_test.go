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
