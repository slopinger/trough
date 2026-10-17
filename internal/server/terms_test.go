package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTermsVersions(t *testing.T) {
	la := startLiveApp(t)
	day := func(d int) string { return time.Now().UTC().AddDate(0, 0, d).Format(dateOnly) }
	la.saveTermsVersions([]TermsVersion{{Date: day(-10), Text: "old"}, {Date: day(5), Text: "next"}, {Date: day(-3), Text: "now"}})

	cur, ok := la.terms("")
	if !ok || cur.Text != "now" || !cur.Effective || cur.Next != day(5) {
		t.Fatalf("current: %+v", cur)
	}
	if old, _ := la.terms(day(-10)); old.Text != "old" || old.Next != day(-3) {
		t.Errorf("old version: %+v", old)
	}
	if next, _ := la.terms(day(5)); next.Effective || next.Next != "" {
		t.Errorf("upcoming version: %+v", next)
	}
	if _, ok := la.terms(day(1)); ok {
		t.Error("a date without a version was found")
	}
	want := []termsRow{{day(5), "upcoming"}, {day(-3), "in effect"}, {day(-10), "previous"}}
	if len(cur.Versions) != len(want) {
		t.Fatalf("versions: %v", cur.Versions)
	}
	for i, r := range cur.Versions {
		if r != want[i] {
			t.Errorf("row %d: %v, want %v", i, r, want[i])
		}
	}

	// before anything is in effect, the first upcoming version is shown
	la.saveTermsVersions([]TermsVersion{{Date: day(7), Text: "soon"}})
	if cur, ok := la.terms(""); !ok || cur.Text != "soon" || cur.Effective {
		t.Errorf("only upcoming: %+v", cur)
	}
}

func TestTermsOfService(t *testing.T) {
	la := startLiveApp(t)
	la.CreateLocalUser("boss", "password123", true)
	get := func(c *http.Client, path string) (int, string) {
		t.Helper()
		resp, err := c.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	api := &apiClient{t: t, base: la.base}

	// no terms until the admin publishes some
	if code, _ := get(http.DefaultClient, "/terms-of-service"); code != 404 {
		t.Errorf("terms page without terms: %d", code)
	}
	if code, _, _ := api.do("GET", "/api/v1/instance/terms_of_service", nil); code != 404 {
		t.Errorf("terms API without terms: %d", code)
	}
	if _, about := get(http.DefaultClient, "/about"); strings.Contains(about, `href="/terms-of-service"`) {
		t.Error("about links terms that don't exist")
	}

	admin := webClient(t, la, "boss", "password123")
	_, page := get(admin, "/admin")
	if !strings.Contains(page, `action="/admin/terms"`) {
		t.Fatal("admin can't publish terms")
	}
	csrf := strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0]
	post := func(v url.Values) {
		t.Helper()
		v.Set("csrf", csrf)
		req, _ := http.NewRequest("POST", la.base+"/admin/terms", strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	today := todayUTC()
	later := time.Now().UTC().AddDate(0, 0, 30).Format(dateOnly)
	post(url.Values{"date": {today}, "text": {"# Be nice\n- no spam <b>"}})
	post(url.Values{"date": {"2020-01-01"}, "text": {"backdated"}}) // refused
	post(url.Values{"date": {later}, "text": {"# Be nicer"}})

	code, body := get(http.DefaultClient, "/terms-of-service")
	if code != 200 || !strings.Contains(body, "<h3>Be nice</h3>") || !strings.Contains(body, "no spam &lt;b&gt;") ||
		!strings.Contains(body, `href="/terms-of-service/`+later+`"`) {
		t.Fatalf("terms page: %d", code)
	}
	if !strings.Contains(body, `href="/terms-of-service"`) {
		t.Error("footer doesn't link the terms")
	}
	m := api.ok("GET", "/api/v1/instance/terms_of_service", nil).(map[string]any)
	if m["effective_date"] != today || m["effective"] != true || m["succeeded_by"] != later || !strings.Contains(m["content"].(string), "<li>no spam") {
		t.Errorf("api current: %v", m)
	}
	m = api.ok("GET", "/api/v1/instance/terms_of_service/"+later, nil).(map[string]any)
	if m["effective"] != false || m["succeeded_by"] != nil || !strings.Contains(m["content"].(string), "Be nicer") {
		t.Errorf("api upcoming: %v", m)
	}
	inst := api.ok("GET", "/api/v2/instance", nil).(map[string]any)
	if u := inst["configuration"].(map[string]any)["urls"].(map[string]any)["terms_of_service"]; u != la.base+"/terms-of-service" {
		t.Errorf("instance urls.terms_of_service: %v", u)
	}
	if code, _, _ := api.do("GET", "/api/v1/instance/terms_of_service/2020-01-01", nil); code != 404 {
		t.Errorf("backdated terms were published: %d", code)
	}

	// upcoming versions can be withdrawn, ones in effect can't
	post(url.Values{"date": {later}, "delete": {"1"}})
	post(url.Values{"date": {today}, "delete": {"1"}})
	if vs := la.termsVersions(); len(vs) != 1 || vs[0].Date != today {
		t.Errorf("after removing: %+v", vs)
	}
}
