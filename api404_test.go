package main

import (
	"net/http"
	"strings"
	"testing"
)

// unknown API paths answer JSON, not the HTML 404 page
func TestAPIUnknownPathJSON(t *testing.T) {
	la := startLiveApp(t)
	for _, p := range []string{"/api/v1/nonexistent", "/api/v3/foo/bar", "/api/v1/statuses/1/nonexistent"} {
		resp, err := http.Get(la.base + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: %d %s", p, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
}
