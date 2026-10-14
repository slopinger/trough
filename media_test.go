package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func orientationEXIF(order binary.ByteOrder, orientation uint16) []byte {
	tiff := make([]byte, 26)
	if order == binary.LittleEndian {
		copy(tiff, "II")
	} else {
		copy(tiff, "MM")
	}
	order.PutUint16(tiff[2:], 42)
	order.PutUint32(tiff[4:], 8)
	order.PutUint16(tiff[8:], 1)
	order.PutUint16(tiff[10:], 0x0112)
	order.PutUint16(tiff[12:], 3)
	order.PutUint32(tiff[14:], 1)
	order.PutUint16(tiff[18:], orientation)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe1, 0, byte(len(payload) + 2)}
	return append(jpeg, payload...)
}

func TestJPEGOrientation(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for orientation := 1; orientation <= 8; orientation++ {
			data := orientationEXIF(order, uint16(orientation))
			if got := jpegOrientation(data); got != orientation {
				t.Fatalf("orientation=%d, got %d", orientation, got)
			}
		}
	}
}

func TestJPEGOrientationMalformed(t *testing.T) {
	cases := [][]byte{
		nil,
		{0xff, 0xd8, 0xff, 0xe1, 0, 0},
		{0xff, 0xd8, 0xff, 0xe1, 0, 1},
		{0xff, 0xd8, 0xff, 0xe1, 0, 20},
		{0xff, 0xd8, 0xff, 0xd9},
	}
	valid := orientationEXIF(binary.LittleEndian, 6)
	// Every truncated prefix should be safe, including a complete orientation
	// entry whose enclosing segment is shorter than its declared length.
	for i := 0; i < len(valid); i++ {
		cases = append(cases, valid[:i])
	}
	for i, data := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if got := jpegOrientation(data); got != 1 {
				t.Fatalf("malformed input returned %d, want default 1", got)
			}
		})
	}
}

func FuzzJPEGOrientation(f *testing.F) {
	f.Add(orientationEXIF(binary.LittleEndian, 6))
	f.Add(orientationEXIF(binary.BigEndian, 8))
	f.Add([]byte{0xff, 0xd8, 0xff, 0xe1, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if got := jpegOrientation(data); got < 1 || got > 8 {
			t.Fatalf("orientation out of range: %d", got)
		}
	})
}

// A failed fetch should stay serialized with requests already waiting for the
// same URL, even when another request arrives after the first one finishes.
func TestProxySerializesFailedFetches(t *testing.T) {
	app := testApp(t)
	app.cfg.Insecure = true
	app.mediaClient = newHTTPClient(true)
	started := make(chan struct{}, 3)
	release := make(chan struct{}, 3)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		http.Error(w, "retry later", http.StatusServiceUnavailable)
	}))
	defer remote.Close()
	defer close(release)
	target := remote.URL + "/image.png"
	fetch := func() <-chan int {
		done := make(chan int, 1)
		go func() {
			req := httptest.NewRequest("GET", "/proxy/"+app.proxySig(target)+"?url="+url.QueryEscape(target), nil)
			req.SetPathValue("sig", app.proxySig(target))
			recorder := httptest.NewRecorder()
			app.handleProxy(recorder, req)
			done <- recorder.Code
		}()
		return done
	}
	awaitStart := func() {
		t.Helper()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("remote request did not start")
		}
	}
	first := fetch()
	awaitStart()
	second := fetch()
	time.Sleep(30 * time.Millisecond) // allow the second request to wait on the first
	release <- struct{}{}
	if got := <-first; got != http.StatusBadGateway {
		t.Fatalf("first response: %d", got)
	}
	awaitStart()
	third := fetch()
	select {
	case <-started:
		t.Error("third request fetched concurrently with the existing waiter")
		release <- struct{}{}
	case <-time.After(100 * time.Millisecond):
	}
	release <- struct{}{}
	if got := <-second; got != http.StatusBadGateway {
		t.Fatalf("second response: %d", got)
	}
	select {
	case <-started:
		release <- struct{}{}
	case <-time.After(100 * time.Millisecond):
	}
	if got := <-third; got != http.StatusBadGateway {
		t.Fatalf("third response: %d", got)
	}
}

func TestProxyRepairsMissingOrInvalidCache(t *testing.T) {
	for _, invalidType := range []bool{false, true} {
		name := "missing body"
		if invalidType {
			name = "invalid metadata"
		}
		t.Run(name, func(t *testing.T) {
			app := testApp(t)
			app.cfg.Insecure = true
			app.mediaClient = newHTTPClient(true)
			var pngData bytes.Buffer
			if err := png.Encode(&pngData, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
				t.Fatal(err)
			}
			var hits atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "image/png")
				w.Write(pngData.Bytes())
			}))
			defer remote.Close()
			target := remote.URL + "/picture.png"
			file := filepath.Join(app.cacheDir, sha256Hex(target))
			metadata := "image/png"
			if invalidType {
				metadata = "not/a-media-type"
				if err := os.WriteFile(file, []byte("bad cache"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(file+".type", []byte(metadata), 0600); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				req := httptest.NewRequest("GET", "/proxy/"+app.proxySig(target)+"?url="+url.QueryEscape(target), nil)
				req.SetPathValue("sig", app.proxySig(target))
				response := httptest.NewRecorder()
				app.handleProxy(response, req)
				if response.Code != http.StatusOK {
					t.Fatalf("response=%d, body=%s", response.Code, response.Body.String())
				}
				if response.Header().Get("Content-Type") != "image/png" || !bytes.Equal(response.Body.Bytes(), pngData.Bytes()) {
					t.Fatal("served corrupt cached data")
				}
			}
			if hits.Load() != 1 {
				t.Fatalf("remote fetches=%d, want 1", hits.Load())
			}
		})
	}
}

func TestProxyRejectsEmptyRemoteMedia(t *testing.T) {
	app := testApp(t)
	app.cfg.Insecure = true
	app.mediaClient = newHTTPClient(true)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", "image/png") }))
	defer remote.Close()
	target := remote.URL + "/empty.png"
	req := httptest.NewRequest("GET", "/proxy/"+app.proxySig(target)+"?url="+url.QueryEscape(target), nil)
	req.SetPathValue("sig", app.proxySig(target))
	response := httptest.NewRecorder()
	app.handleProxy(response, req)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("response=%d, want 502", response.Code)
	}
}

func TestCacheCleanupWaitsForFreshDownload(t *testing.T) {
	app := testApp(t)
	app.cfg.Insecure = true
	app.mediaClient = newHTTPClient(true)
	started := make(chan struct{})
	release := make(chan struct{})
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "image/png")
		w.Write(picture.Bytes())
	}))
	defer remote.Close()
	defer close(release)
	target := remote.URL + "/picture.png"
	file := filepath.Join(app.cacheDir, sha256Hex(target))
	old := time.Now().Add(-48 * time.Hour)
	for name, content := range map[string][]byte{file: []byte("old media"), file + ".type": []byte("invalid")} {
		if err := os.WriteFile(name, content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, old, old); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest("GET", "/proxy/"+app.proxySig(target)+"?url="+url.QueryEscape(target), nil)
		req.SetPathValue("sig", app.proxySig(target))
		response := httptest.NewRecorder()
		app.handleProxy(response, req)
		done <- response
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("download did not start")
	}
	cleaned := make(chan struct{})
	go func() { app.cleanCache(24 * time.Hour); close(cleaned) }()
	select {
	case <-cleaned:
		t.Fatal("cleanup bypassed the active download lock")
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case response := <-done:
		if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), picture.Bytes()) {
			t.Fatal("download failed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("download did not finish")
	}
	select {
	case <-cleaned:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not finish")
	}
	for _, name := range []string{file, file + ".type"} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("fresh cache entry removed: %v", err)
		}
	}
}
