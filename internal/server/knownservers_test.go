package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestKnownServers(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	la.CreateLocalUser("boss", "password123", true)
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	r1, r2 := newRemote(t, la), newRemote(t, la)
	r1.put("/users/alice", mastodonActor)
	r2.put("/users/alice", mastodonActor)
	alice, _ := la.getAccount(ctx, r1.Base+"/users/alice")
	other, _ := la.getAccount(ctx, r2.Base+"/users/alice")
	la.db.Exec(`INSERT INTO follows (id,uri,account_id,target_id,accepted,created_at) VALUES ('f1','x1',?,?,1,0)`, bob.ID, alice.ID)
	la.db.Exec(`INSERT INTO statuses (id,uri,account_id,visibility,created_at) VALUES ('s1',?,?,'public',?)`, r1.Base+"/s/1", alice.ID, time.Now().Unix())
	la.db.Exec(`INSERT INTO deliveries (account_id,inbox,payload,attempts,next_try,created_at) VALUES (?,?,'{}',2,0,0)`, bob.ID, r1.Base+"/inbox")
	la.limitServer(r2.Host, "")

	byDomain := map[string]*KnownServer{}
	for _, s := range la.knownServers() {
		byDomain[s.Domain] = s
	}
	s1, s2 := byDomain[alice.Domain], byDomain[other.Domain]
	if s1 == nil || s1.Accounts != 1 || s1.Following != 1 || s1.Followers != 0 || s1.Posts != 1 || s1.FailingDeliveries != 1 || s1.Severity != "" {
		t.Fatalf("first server: %+v", s1)
	}
	if s2 == nil || s2.Severity != "limit" || s2.Following != 0 {
		t.Fatalf("second server: %+v", s2)
	}

	// the page lists them, most connected first, and finds one by name
	admin := webClient(t, la, "boss", "password123")
	get := func(path string) string {
		resp, err := admin.Get(la.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	page := get("/admin/servers")
	if i, j := strings.Index(page, alice.Domain), strings.Index(page, other.Domain); i < 0 || j < 0 || i > j {
		t.Error("servers missing or not ordered by connections")
	}
	if !strings.Contains(page, "1 failing delivery") || !strings.Contains(page, "limited") {
		t.Error("page lacks the delivery or moderation state")
	}
	if page := get("/admin/servers?q=" + url.QueryEscape(other.Domain)); strings.Contains(page, `<b>`+alice.Domain+`</b>`) {
		t.Error("search didn't narrow the list")
	}

	// blocking from the list comes back to it
	csrf := strings.SplitN(strings.SplitN(page, `name="csrf" value="`, 2)[1], `"`, 2)[0]
	admin.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, _ := http.NewRequest("POST", la.base+"/admin/domain_blocks", strings.NewReader(url.Values{"csrf": {csrf}, "domain": {alice.Domain},
		"severity": {"suspend"}, "back": {"servers"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", la.base)
	resp, err := admin.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/admin/servers?q=") || !la.domainBlocked(alice.Domain) {
		t.Errorf("block from the list: %s, blocked %v", loc, la.domainBlocked(alice.Domain))
	}
}
