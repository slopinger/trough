package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAppearance(t *testing.T) {
	la := startLiveApp(t)
	la.CreateLocalUser("bob", "password123", false)
	c := webClient(t, la, "bob", "password123")
	page := func() string {
		resp, err := c.Get(la.base + "/settings")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	if p := page(); !strings.Contains(p, `<html lang="en">`) {
		t.Fatal("default should follow the device")
	}
	csrf := strings.SplitN(strings.SplitN(page(), `name="csrf" value="`, 2)[1], `"`, 2)[0]
	form := url.Values{"csrf": {csrf}, "theme": {"dark"}, "text_size": {"large"}}
	req, _ := http.NewRequest("POST", la.base+"/settings/appearance", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", la.base)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if p := page(); !strings.Contains(p, `<html lang="en" data-theme="dark" data-size="large">`) {
		t.Fatal("appearance not applied")
	}
}
