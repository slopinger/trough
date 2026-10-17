package server

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServerAppearance(t *testing.T) {
	la := startLiveApp(t)
	la.CreateLocalUser("boss", "password123", true)
	admin := webClient(t, la, "boss", "password123")
	get := func(c *http.Client, path string) (*http.Response, string) {
		t.Helper()
		resp, err := c.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	post := func(fields map[string]string, file []byte) {
		t.Helper()
		_, page := get(admin, "/admin")
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		mw.WriteField("csrf", strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0])
		for k, v := range fields {
			mw.WriteField(k, v)
		}
		if file != nil {
			fw, _ := mw.CreateFormFile("thumbnail", "banner.png")
			fw.Write(file)
		}
		mw.Close()
		req, _ := http.NewRequest("POST", la.base+"/admin/appearance", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Origin", la.base)
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for x := 0; x < 400; x++ {
		for y := 0; y < 200; y++ {
			img.Set(x, y, color.RGBA{uint8(x), 120, uint8(y), 255})
		}
	}
	var pngData bytes.Buffer
	png.Encode(&pngData, img)

	// the thumbnail shows on the start and about pages and in the instance API
	post(map[string]string{}, pngData.Bytes())
	thumb := la.getSetting("thumbnail")
	if !strings.HasPrefix(thumb, la.base+"/media/") {
		t.Fatalf("thumbnail setting %q", thumb)
	}
	api := &apiClient{t: t, base: la.base}
	if inst := api.ok("GET", "/api/v2/instance", nil).(map[string]any); inst["thumbnail"].(map[string]any)["url"] != thumb {
		t.Errorf("v2 thumbnail: %v", inst["thumbnail"])
	}
	for _, path := range []string{"/", "/about"} {
		if _, page := get(http.DefaultClient, path); !strings.Contains(page, `class="server-thumb"`) {
			t.Errorf("%s lacks the thumbnail", path)
		}
	}

	// custom CSS is linked from every page
	post(map[string]string{"save_css": "1", "custom_css": ":root { --accent: #c2410c; }"}, nil)
	resp, css := get(http.DefaultClient, "/custom.css")
	if css != ":root { --accent: #c2410c; }" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") {
		t.Errorf("custom.css: %q %s", css, resp.Header.Get("Content-Type"))
	}
	if _, page := get(http.DefaultClient, "/about"); !strings.Contains(page, `href="/custom.css?v=`) {
		t.Error("pages don't link the custom CSS")
	}

	// removing the thumbnail deletes the file and falls back to the logo
	post(map[string]string{"remove_thumbnail": "1"}, nil)
	if _, err := os.Stat(filepath.Join(la.mediaDir, filepath.Base(thumb))); !os.IsNotExist(err) {
		t.Error("thumbnail file left behind")
	}
	if inst := api.ok("GET", "/api/v1/instance", nil).(map[string]any); !strings.HasSuffix(inst["thumbnail"].(string), "/static/favicon.svg") {
		t.Errorf("v1 thumbnail after removal: %v", inst["thumbnail"])
	}

	// only admins
	la.CreateLocalUser("bob", "password123", false)
	if resp, _ := get(webClient(t, la, "bob", "password123"), "/admin"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("bob opened /admin: %d", resp.StatusCode)
	}
}
