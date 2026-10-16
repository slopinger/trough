package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func box(typ string, parts ...[]byte) []byte {
	payload := bytes.Join(parts, nil)
	b := binary.BigEndian.AppendUint32(nil, uint32(8+len(payload)))
	return append(append(b, typ...), payload...)
}

// testMP4 builds a minimal MP4 with one video track of w x h and a
// location in the movie's user data, as phones write it.
func testMP4(w, h int) []byte {
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[76:], uint32(w)<<16)
	binary.BigEndian.PutUint32(tkhd[80:], uint32(h)<<16)
	hdlr := append(make([]byte, 8), "vide"...)
	hdlr = append(hdlr, make([]byte, 13)...)
	moov := box("moov",
		box("mvhd", make([]byte, 100)),
		box("trak", box("tkhd", tkhd), box("mdia", box("hdlr", hdlr))),
		box("udta", box("\xa9xyz", []byte("\x00\x12\x15\xc7+52.5200+013.4050/"))),
		box("meta", []byte("com.apple.quicktime.location.ISO6709")))
	return bytes.Join([][]byte{box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2avc1mp41")), moov, box("mdat", []byte("frames"))}, nil)
}

func TestSniffAV(t *testing.T) {
	mp4 := testMP4(640, 360)
	size := len(mp4)
	info, err := sniffAV(mp4)
	if err != nil || info.mediaType != "video/mp4" || info.ext != ".mp4" || info.width != 640 || info.height != 360 {
		t.Fatalf("mp4 = %+v, %v", info, err)
	}
	if len(mp4) != size || bytes.Contains(mp4, []byte("52.5200")) || bytes.Contains(mp4, []byte("ISO6709")) || bytes.Contains(mp4, []byte("udta")) {
		t.Fatal("location metadata left in the file")
	}
	if !bytes.Contains(mp4, []byte("frames")) || !mp4Valid(mp4) {
		t.Fatal("stripping broke the file")
	}

	cases := map[string][]byte{
		"audio/mpeg": append([]byte("ID3\x04\x00\x00"), make([]byte, 32)...),
		"audio/ogg":  []byte("OggS\x00\x02rest"),
		"audio/wav":  []byte("RIFF\x24\x00\x00\x00WAVEfmt "),
		"audio/flac": []byte("fLaC\x00\x00\x00\x22"),
		"video/webm": append([]byte{0x1A, 0x45, 0xDF, 0xA3}, "\x42\x82\x84webm...\x86\x85V_VP9"...),
		"audio/webm": append([]byte{0x1A, 0x45, 0xDF, 0xA3}, "\x42\x82\x84webm...\x86\x86A_OPUS"...),
	}
	for want, data := range cases {
		if info, err := sniffAV(data); err != nil || info.mediaType != want {
			t.Errorf("%s: %+v %v", want, info, err)
		}
	}
	for name, data := range map[string][]byte{
		"text":         []byte("<html>not a video</html>"),
		"mkv":          append([]byte{0x1A, 0x45, 0xDF, 0xA3}, "\x42\x82\x88matroska"...),
		"truncated":    testMP4(10, 10)[:60],
		"no mdat":      append(box("ftyp", []byte("isom")), box("moov")...),
		"huge box":     append(box("ftyp", []byte("isom")), 0x7f, 0xff, 0xff, 0xff, 'm', 'o', 'o', 'v'),
		"64-bit lying": append(box("ftyp", []byte("isom")), append([]byte{0, 0, 0, 1, 'm', 'd', 'a', 't'}, 0xff, 0, 0, 0, 0, 0, 0, 0)...),
	} {
		if info, err := sniffAV(data); err == nil {
			t.Errorf("%s accepted as %+v", name, info)
		}
	}
}

func FuzzSniffAV(f *testing.F) {
	f.Add(testMP4(320, 240))
	f.Add([]byte("ID3"))
	f.Fuzz(func(t *testing.T, data []byte) { sniffAV(data) })
}

func TestVideoUpload(t *testing.T) {
	la, _, _, _ := featureFixture(t)
	token := oauthLogin(t, la, "bob", "password123", "read write")
	c := &apiClient{t: t, base: la.base, token: token}
	upload := func(path, field string, data []byte) (int, map[string]any) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile(field, "clip.mp4")
		fw.Write(data)
		mw.WriteField("description", "a cat jumping")
		mw.Close()
		method := "POST"
		if strings.Contains(path, "update_credentials") {
			method = "PATCH"
		}
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
		return resp.StatusCode, m
	}

	code, media := upload("/api/v2/media", "file", testMP4(1280, 720))
	if code != 200 || media["type"] != "video" || !strings.HasSuffix(media["url"].(string), ".mp4") {
		t.Fatalf("upload = %d %v", code, media)
	}
	if orig := media["meta"].(map[string]any)["original"].(map[string]any); orig["width"] != 1280.0 || orig["height"] != 720.0 {
		t.Errorf("meta = %v", media["meta"])
	}

	req, _ := http.NewRequest("GET", media["url"].(string), nil)
	req.Header.Set("Range", "bytes=0-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || resp.Header.Get("Content-Type") != "video/mp4" || len(head) != 8 {
		t.Errorf("range request = %d %q %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(head))
	}

	st := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"look"}, "media_ids[]": {media["id"].(string)}}).(map[string]any)
	req, _ = http.NewRequest("GET", st["uri"].(string), nil)
	req.Header.Set("Accept", "application/activity+json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var note map[string]any
	json.NewDecoder(resp.Body).Decode(&note)
	resp.Body.Close()
	att, _ := note["attachment"].([]any)
	if len(att) != 1 || att[0].(map[string]any)["mediaType"] != "video/mp4" || att[0].(map[string]any)["width"] != 1280.0 {
		t.Errorf("federated attachment = %v", note["attachment"])
	}

	if code, _ := upload("/api/v1/accounts/update_credentials", "avatar", testMP4(64, 64)); code != 422 {
		t.Errorf("video avatar accepted: %d", code)
	}
	if code, _ := upload("/api/v2/media", "file", []byte("<svg onload=alert(1)>")); code != 422 {
		t.Errorf("svg accepted: %d", code)
	}
}
