package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestPolicyHTML(t *testing.T) {
	got := policyHTML("# Data <stuff>\nfirst line\nsecond\n\n- one & two\n- three\nafter\n\n\n# End")
	want := "<h3>Data &lt;stuff&gt;</h3><p>first line<br>second</p><ul><li>one &amp; two</li><li>three</li></ul><p>after</p><h3>End</h3>"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestPrivacyPolicy(t *testing.T) {
	la := startLiveApp(t)
	la.CreateLocalUser("boss", "password123", true)
	get := func(c *http.Client, path string) string {
		t.Helper()
		resp, err := c.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	// the default describes trough, linked from every page
	if p := get(http.DefaultClient, "/privacy-policy"); !strings.Contains(p, "<h3>What is stored about you</h3>") ||
		!strings.Contains(p, "runs trough") || !strings.Contains(p, `href="/privacy-policy"`) {
		t.Fatal("default policy page wrong")
	}
	api := &apiClient{t: t, base: la.base}
	if m := api.ok("GET", "/api/v1/instance/privacy_policy", nil).(map[string]any); !strings.Contains(m["content"].(string), "<li>") || m["updated_at"] == "" {
		t.Errorf("api: %v", m)
	}

	admin := webClient(t, la, "boss", "password123")
	page := get(admin, "/admin")
	if !strings.Contains(page, `name="privacy_policy"`) || !strings.Contains(page, "This is trough's default") {
		t.Fatal("admin can't edit the policy")
	}
	csrf := strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0]
	save := func(text string) {
		req, _ := http.NewRequest("POST", la.base+"/admin/privacy_policy", strings.NewReader(url.Values{"csrf": {csrf}, "privacy_policy": {text}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	save("# Ours\nWe keep backups for 14 days. <script>")
	if p := get(http.DefaultClient, "/privacy-policy"); !strings.Contains(p, "<h3>Ours</h3>") || strings.Contains(p, "<script>") || strings.Contains(p, "runs trough") {
		t.Error("custom policy not shown, or not escaped")
	}
	save("")
	if p := get(http.DefaultClient, "/privacy-policy"); !strings.Contains(p, "runs trough") {
		t.Error("emptying didn't restore the default")
	}
}
