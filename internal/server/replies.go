package server

import (
	"context"
	"sync"
	"time"

	"git.freifick.net/slop/trough/internal/ap"
)

// Fetching the replies of remote threads, like Mastodon 4.4: when a signed-in
// user opens a thread whose root is remote, the replies its server lists in
// the post's `replies` collection (and their replies, a few levels down) are
// fetched in the background. Each reply is fetched from its own origin and
// only kept if it really answers the post it was listed under.

const (
	repliesEvery = 15 * time.Minute // per thread
	repliesMax   = 60               // posts fetched per refresh
	repliesDepth = 3                // levels below the root
	repliesPages = 3                // collection pages per post
)

var (
	repliesMu   sync.Mutex
	repliesSeen = map[string]time.Time{}
	repliesSem  = make(chan struct{}, 2)
)

// fetchRepliesLater starts fetching the replies of st's thread, at most once
// per repliesEvery. It reports whether a fetch was started.
func (app *App) fetchRepliesLater(st *Status) bool {
	root := st
	if id := app.threadRoot(st.ID); id != "" && id != st.ID {
		if r, err := app.StatusByID(id); err == nil {
			root = r
		}
	}
	if root.Local || root.URI == "" {
		return false
	}
	repliesMu.Lock()
	if t, ok := repliesSeen[root.ID]; ok && time.Since(t) < repliesEvery {
		repliesMu.Unlock()
		return false
	}
	repliesSeen[root.ID] = time.Now()
	for id, t := range repliesSeen { // keep the map small
		if time.Since(t) > repliesEvery {
			delete(repliesSeen, id)
		}
	}
	repliesMu.Unlock()
	go func() {
		repliesSem <- struct{}{}
		defer func() { <-repliesSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		app.fetchReplies(ctx, root.URI)
	}()
	return true
}

// fetchReplies walks the replies collections below rootURI breadth first.
func (app *App) fetchReplies(ctx context.Context, rootURI string) int {
	root, err := app.fetchAP(ctx, rootURI)
	if err != nil || ap.Str(root, "id") != rootURI {
		return 0
	}
	type item struct {
		note  obj
		depth int
	}
	queue := []item{{root, 0}}
	fetched, stored := 0, 0
	for len(queue) > 0 && fetched < repliesMax && ctx.Err() == nil {
		it := queue[0]
		queue = queue[1:]
		parent := ap.Str(it.note, "id")
		for _, iri := range app.collectionIRIs(ctx, it.note["replies"], parent) {
			if fetched >= repliesMax {
				break
			}
			_, err := app.StatusByURI(iri)
			have := err == nil
			if have && it.depth+1 >= repliesDepth {
				continue // nothing new to learn down there
			}
			fetched++
			note, err := app.fetchAP(ctx, iri)
			if err != nil || ap.Str(note, "id") != iri || ap.ID(note["inReplyTo"]) != parent {
				continue // not a reply to this post
			}
			if !have {
				if _, err := app.storeFetchedNote(ctx, note, 4); err == nil {
					stored++
				}
			}
			if it.depth+1 < repliesDepth {
				queue = append(queue, item{note, it.depth + 1})
			}
		}
	}
	return stored
}

// collectionIRIs lists the item IRIs of a replies collection (inline or by
// IRI, following first/next pages). Collections must be on the post's host;
// items are only IRIs here, the posts themselves are fetched from their origin.
func (app *App) collectionIRIs(ctx context.Context, coll any, owner string) []string {
	var out []string
	seen := map[string]bool{}
	page, pages, usedFirst := coll, 0, false
	for page != nil && pages <= repliesPages && len(out) < repliesMax {
		m, ok := page.(obj)
		if !ok {
			iri := ap.ID(page)
			if iri == "" || !ap.SameHost(iri, owner) || seen[iri] {
				break
			}
			seen[iri] = true
			fetchedPage, err := app.fetchAP(ctx, iri)
			if err != nil || !ap.SameHost(ap.Str(fetchedPage, "id"), owner) {
				break
			}
			m = fetchedPage
		}
		pages++
		for _, key := range []string{"orderedItems", "items"} {
			list, _ := m[key].([]any)
			for _, x := range list {
				if iri := ap.ID(x); iri != "" && !app.isLocalIRI(iri) {
					out = append(out, iri)
				}
			}
		}
		// a Collection without items starts at its first page
		if first, ok := m["first"]; ok && !usedFirst && m["orderedItems"] == nil && m["items"] == nil {
			page, usedFirst = first, true
			continue
		}
		page = m["next"]
	}
	return out
}
