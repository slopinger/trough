package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slopinger/trough/internal/ap"
)

func TestVideoPosters(t *testing.T) {
	la, r, _, alice := featureFixture(t)
	token := oauthLogin(t, la, "bob", "password123", "read write")
	c := &apiClient{t: t, base: la.base, token: token}
	send := func(method, path string, files map[string][]byte, fields map[string]string) map[string]any {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for name, data := range files {
			fw, _ := mw.CreateFormFile(name, name+".bin")
			fw.Write(data)
		}
		for k, v := range fields {
			mw.WriteField(k, v)
		}
		mw.Close()
		req, _ := http.NewRequest(method, la.base+path, &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		if resp.StatusCode != 200 {
			t.Fatalf("%s %s = %d %v", method, path, resp.StatusCode, m)
		}
		return m
	}
	exists := func(u string) bool {
		_, err := os.Stat(filepath.Join(la.mediaDir, filepath.Base(u)))
		return err == nil
	}

	// a video without a thumbnail has no preview image (it used to claim the video itself)
	plain := send("POST", "/api/v2/media", map[string][]byte{"file": testMP4(640, 360)}, nil)
	if plain["preview_url"] != nil {
		t.Errorf("preview_url without poster = %v", plain["preview_url"])
	}
	// with one, preview_url is the re-encoded image
	media := send("POST", "/api/v2/media", map[string][]byte{"file": testMP4(640, 360), "thumbnail": pngBytes()}, map[string]string{"description": "a cat jumping"})
	poster, _ := media["preview_url"].(string)
	if poster == "" || poster == media["url"] || !strings.HasSuffix(poster, ".png") || !exists(poster) {
		t.Fatalf("poster = %v", media)
	}
	// replacing it removes the old file and keeps the description
	updated := send("PUT", "/api/v1/media/"+media["id"].(string), map[string][]byte{"thumbnail": pngBytes()}, nil)
	newPoster, _ := updated["preview_url"].(string)
	if newPoster == poster || !exists(newPoster) || exists(poster) || updated["description"] != "a cat jumping" {
		t.Errorf("replaced poster: %v (old file kept: %v)", updated, exists(poster))
	}
	// images keep themselves as preview; a thumbnail for them is ignored
	if img := send("POST", "/api/v2/media", map[string][]byte{"file": pngBytes(), "thumbnail": pngBytes()}, nil); img["preview_url"] != img["url"] {
		t.Errorf("image preview_url = %v", img["preview_url"])
	}

	st := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"look"}, "media_ids[]": {media["id"].(string)}}).(map[string]any)
	req, _ := http.NewRequest("GET", st["uri"].(string), nil)
	req.Header.Set("Accept", "application/activity+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var note map[string]any
	json.NewDecoder(resp.Body).Decode(&note)
	resp.Body.Close()
	if att, _ := note["attachment"].([]any); len(att) != 1 || ap.URL(ap.Object(att[0].(map[string]any)["icon"])["url"]) != newPoster {
		t.Errorf("federated attachment has no poster icon: %v", note["attachment"])
	}
	page := func(path string) string {
		resp, err := http.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		// the compose form reads a frame from the chosen file through a blob: URL
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "media-src 'self' blob:") {
			t.Errorf("CSP blocks local video previews: %s", csp)
		}
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if !strings.Contains(page("/s/"+st["id"].(string)), `poster="/media/`+filepath.Base(newPoster)+`"`) {
		t.Error("web page doesn't show the poster")
	}

	// incoming: Mastodon puts a video's thumbnail in the Document's icon
	nid := r.Base + "/users/alice/statuses/v"
	remoteNote := obj{"id": nid, "type": "Note", "attributedTo": alice.URI, "content": "<p>clip</p>", "to": []any{ap.Public}, "published": ap.Time(time.Now()),
		"attachment": []any{obj{"type": "Document", "mediaType": "video/mp4", "url": r.Base + "/system/clip.mp4", "name": "a clip",
			"icon": obj{"type": "Image", "mediaType": "image/jpeg", "url": r.Base + "/system/clip-thumb.jpg"}}}}
	r.send(obj{"@context": asContext, "id": nid + "/a", "type": "Create", "actor": alice.URI, "to": []any{ap.Public}, "object": remoteNote}, r.Base+"/users/alice#main-key", sigCavage)
	var rs *Status
	eventually(t, "remote video stored", func() bool { rs, _ = la.StatusByURI(nid); return rs != nil })
	got := c.ok("GET", "/api/v1/statuses/"+rs.ID, nil).(map[string]any)["media_attachments"].([]any)[0].(map[string]any)
	if got["preview_remote_url"] != r.Base+"/system/clip-thumb.jpg" || got["preview_url"] == nil || got["preview_url"] == got["url"] {
		t.Errorf("remote poster = %v", got)
	}

	// deleting the post keeps the media (and poster) for a redraft; the orphan cleanup removes both later
	c.ok("DELETE", "/api/v1/statuses/"+st["id"].(string), nil)
	if !exists(newPoster) {
		t.Fatal("poster removed before a redraft could use it")
	}
	la.db.Exec(`UPDATE attachments SET created_at=0 WHERE id=?`, media["id"])
	la.cleanOrphanMedia()
	if exists(newPoster) || exists(media["url"].(string)) {
		t.Error("orphan cleanup left the video or its poster")
	}
}
