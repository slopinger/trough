package server

import (
	"testing"
	"time"

	"git.freifick.net/slop/trough/internal/ulid"
)

func threadFixtures(t *testing.T, parents []int) (*App, []*Status) {
	t.Helper()
	app := testApp(t)
	author, err := app.CreateLocalUser("threadtest", "test-password", false)
	if err != nil {
		t.Fatal(err)
	}
	statuses := make([]*Status, len(parents))
	for i := range statuses {
		id := ulid.New()
		statuses[i] = &Status{ID: id, URI: app.statusURI(author.Username, id), URL: app.statusURL(author.Username, id), AccountID: author.ID, Local: true, Visibility: "public", CreatedAt: time.Now()}
	}
	for i, parent := range parents {
		if parent >= 0 {
			statuses[i].InReplyToID = statuses[parent].ID
			statuses[i].InReplyToURI = statuses[parent].URI
			statuses[i].InReplyToAccountID = author.ID
		}
		if err := app.insertStatus(statuses[i]); err != nil {
			t.Fatal(err)
		}
	}
	return app, statuses
}

func TestThreadReplyCycles(t *testing.T) {
	for _, tc := range []struct {
		name      string
		parents   []int
		ancestors int
	}{
		{"self", []int{0}, 0},
		{"two posts", []int{1, 0}, 1},
		{"three posts", []int{1, 2, 0}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, statuses := threadFixtures(t, tc.parents)
			anc, desc := app.thread(nil, statuses[0])
			if len(anc) != tc.ancestors || len(desc) != 0 {
				t.Fatalf("got %d ancestors, %d descendants", len(anc), len(desc))
			}
			seen := map[string]bool{statuses[0].ID: true}
			for _, s := range append(anc, desc...) {
				if seen[s.ID] {
					t.Fatalf("thread repeated %s", s.ID)
				}
				seen[s.ID] = true
			}
		})
	}
}

func TestThreadBranching(t *testing.T) {
	// grandparent -> parent -> root -> two children -> grandchild
	app, statuses := threadFixtures(t, []int{-1, 0, 1, 2, 2, 3})
	anc, desc := app.thread(nil, statuses[2])
	if len(anc) != 2 || anc[0].ID != statuses[0].ID || anc[1].ID != statuses[1].ID {
		t.Fatalf("incorrect ancestors: %v", anc)
	}
	if len(desc) != 3 {
		t.Fatalf("got %d descendants", len(desc))
	}
	depths := map[string]int{}
	for _, s := range desc {
		depths[s.ID] = s.Depth
	}
	if depths[statuses[3].ID] != 0 || depths[statuses[4].ID] != 0 || depths[statuses[5].ID] != 1 {
		t.Fatalf("incorrect depths: %v", depths)
	}
}

func TestThreadDepthLimit(t *testing.T) {
	parents := make([]int, 100)
	for i := range parents {
		parents[i] = i - 1
	}
	app, statuses := threadFixtures(t, parents)
	_, desc := app.thread(nil, statuses[0])
	if len(desc) != 40 {
		t.Fatalf("got %d descendants, want 40", len(desc))
	}
	anc, _ := app.thread(nil, statuses[len(statuses)-1])
	if len(anc) != 40 {
		t.Fatalf("got %d ancestors, want 40", len(anc))
	}
}
