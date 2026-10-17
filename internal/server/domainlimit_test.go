package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.freifick.net/slop/trough/internal/ap"
)

func TestLimitServer(t *testing.T) {
	la := startLiveApp(t)
	ctx := context.Background()
	bob, _ := la.CreateLocalUser("bob", "password123", false)
	carol, _ := la.CreateLocalUser("carol", "password123", false)
	r := newRemote(t, la)
	r.put("/users/alice", mastodonActor)
	alice, _ := la.getAccount(ctx, r.Base+"/users/alice")
	la.db.Exec(`INSERT INTO follows (id,uri,account_id,target_id,accepted,created_at) VALUES ('f1','x1',?,?,1,0)`, carol.ID, alice.ID)

	if err := la.limitServer(r.Host, "low effort spam"); err != nil {
		t.Fatal(err)
	}
	// new posts still arrive, but only followers see them in public timelines
	nid := r.Base + "/users/alice/statuses/1"
	note := obj{"id": nid, "type": "Note", "attributedTo": alice.URI, "content": "<p>buy #coins</p>", "to": []any{ap.Public}, "published": ap.Time(time.Now())}
	r.send(obj{"@context": asContext, "id": nid + "/a", "type": "Create", "actor": alice.URI, "to": []any{ap.Public}, "object": note}, r.Base+"/users/alice#main-key", sigCavage)
	var st *Status
	eventually(t, "remote post stored", func() bool { st, _ = la.StatusByURI(nid); return st != nil })
	has := func(sts []*Status) bool {
		for _, s := range sts {
			if s.ID == st.ID {
				return true
			}
		}
		return false
	}
	if sts, _ := la.federatedTimeline(bob, ""); has(sts) {
		t.Error("bob sees the limited server's post")
	}
	if sts, _ := la.federatedTimeline(carol, ""); !has(sts) {
		t.Error("carol, a follower, lost it")
	}
	if sts, _ := la.tagTimeline(nil, "coins", ""); has(sts) {
		t.Error("visitors see it on the hashtag page")
	}
	if alice, _ = la.AccountByID(alice.ID); alice.Limited || !alice.IsLimited() {
		t.Errorf("limited %v, via server %v", alice.Limited, alice.DomainLimited)
	}
	// its notifications to non-followers are filtered, follows stay
	la.notify(bob.ID, "mention", alice.ID, st.ID)
	if n := la.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=? AND filtered=1`, bob.ID); n != 1 {
		t.Errorf("bob's notification wasn't filtered (%d)", n)
	}
	if n := la.countInt(`SELECT COUNT(*) FROM follows`); n != 1 {
		t.Error("limiting removed follows")
	}
	resp, _ := http.Get(la.base + "/api/v1/instance/domain_blocks")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"severity":"silence"`) {
		t.Errorf("instance domain blocks: %s", b)
	}
	if err := la.BlockServer(r.Host, ""); err != nil || la.countInt(`SELECT COUNT(*) FROM domain_limits`) != 0 {
		t.Errorf("blocking didn't replace the limit: %v", err)
	}
	if err := la.limitServer(r.Host, ""); err == nil {
		t.Error("limited a blocked server")
	}
}
