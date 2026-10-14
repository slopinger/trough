package main

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
