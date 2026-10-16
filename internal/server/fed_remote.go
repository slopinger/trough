package server

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"mime"
	"path"
	"strings"
	"sync"
	"time"

	"git.freifick.net/slop/trough/internal/ap"
	"git.freifick.net/slop/trough/internal/ulid"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// bestImage returns the largest of a post's image/icon objects.
func bestImage(note obj) obj {
	var best obj
	bestW := -1.0
	for _, k := range []string{"image", "icon"} {
		for _, im := range ap.Objects(note[k]) {
			w, _ := im["width"].(float64)
			if ap.URL(im["url"]) != "" && w > bestW {
				best, bestW = obj{"type": "Image", "url": ap.URL(im["url"]), "mediaType": ap.Str(im, "mediaType")}, w
			}
		}
		if best != nil {
			return best
		}
	}
	return nil
}

func attributedToIncludes(note obj, iri string) bool {
	_, ok := attributedTo(note, iri)
	return ok
}

// storeNote saves a remote post (Note/Article/...) by author.
func (app *App) storeNote(ctx context.Context, author *Account, note obj, depth int) (*Status, error) {
	uri := ap.Str(note, "id")
	if uri == "" {
		return nil, errors.New("note without id")
	}
	if st, err := app.StatusByURI(uri); err == nil {
		return st, nil
	}
	published := ap.ParseTime(ap.Str(note, "published"))
	if published.IsZero() || published.After(time.Now()) {
		published = time.Now()
	}
	st := &Status{
		ID:           ulid.At(published),
		URI:          uri,
		URL:          ap.URL(note["url"]),
		AccountID:    author.ID,
		InReplyToURI: ap.ID(note["inReplyTo"]),
		Visibility:   ap.Visibility(note, author.FollowersURI),
		CreatedAt:    published,
	}
	if st.URL == "" || !strings.HasPrefix(st.URL, "http") {
		st.URL = uri
	}
	app.applyNote(st, note)
	st.QuotePolicy = remoteQuotePolicy(note, author)
	if st.InReplyToURI != "" {
		parent, err := app.StatusByURI(st.InReplyToURI)
		if err != nil && depth < 5 {
			parent, err = app.resolveStatusDepth(ctx, st.InReplyToURI, depth+1)
		}
		if err == nil {
			st.InReplyToID, st.InReplyToAccountID = parent.ID, parent.AccountID
		}
	}
	st.QuoteState = app.localQuoteState(st, note)
	if err := app.insertStatus(st); err != nil {
		if existing, err2 := app.StatusByURI(uri); err2 == nil {
			return existing, nil
		}
		return nil, err
	}
	app.db.Exec(`UPDATE statuses SET in_reply_to_id=?, in_reply_to_account_id=? WHERE in_reply_to_uri=? AND in_reply_to_id=''`,
		st.ID, st.AccountID, st.URI)
	if st.QuoteURI != "" && depth < 5 {
		if _, err := app.StatusByURI(st.QuoteURI); err != nil {
			app.resolveStatusDepth(ctx, st.QuoteURI, depth+1)
		}
		if q, err := app.StatusByURI(st.QuoteURI); err == nil && q.Local && st.QuoteState == "" {
			app.notify(q.AccountID, "quote", author.ID, st.ID)
		}
	}
	mentioned := app.replaceNoteExtras(ctx, st, note)
	app.queueCard(st)
	app.publishStatus(st.ID, false)
	if time.Since(st.CreatedAt) < 15*time.Minute { // new posts, not backfilled or fetched old ones
		app.notifySubscribers(st)
	}
	for id := range mentioned {
		app.notify(id, "mention", author.ID, st.ID)
	}
	if st.InReplyToAccountID != "" && !mentioned[st.InReplyToAccountID] {
		if p, err := app.AccountByID(st.InReplyToAccountID); err == nil && p.IsLocal() {
			app.notify(p.ID, "mention", author.ID, st.ID)
		}
	}
	return st, nil
}

// applyNote copies the content fields of a note onto st.
func (app *App) applyNote(st *Status, note obj) {
	content := ap.Str(note, "content")
	if cm, ok := note["contentMap"].(obj); ok {
		for lang, v := range cm {
			if s, ok := v.(string); ok {
				if content == "" {
					content = s
				}
				if st.Language == "" && len(lang) <= 10 {
					st.Language = lang
				}
			}
		}
	}
	if strings.HasPrefix(ap.Str(note, "mediaType"), "text/markdown") && !strings.Contains(content, "</") {
		content = markdownToHTML(content) // PeerTube video descriptions
	}
	name := ap.Str(note, "name")
	summary := ap.Str(note, "summary")
	longform := false
	switch ap.Type(note) {
	case "Article", "Page", "Event", "Video", "Audio", "Image":
		longform = name != ""
		if longform {
			link := ap.URL(note["url"])
			if link == "" {
				link = ap.Str(note, "id")
			}
			lead := ""
			if summary != "" {
				// for articles the summary is a teaser, not a content warning
				lead = "<p><em>" + html.EscapeString(plainText(summary)) + "</em></p>"
				summary = ""
			}
			content = `<p><strong><a href="` + html.EscapeString(link) + `">` + html.EscapeString(name) + `</a></strong></p>` + lead + content
		}
		// link posts (Lemmy): the link is an attachment, not in the text
		for _, a := range ap.Objects(note["attachment"]) {
			if ap.Type(a) == "Link" && !strings.HasPrefix(ap.Str(a, "mediaType"), "image/") {
				if href := ap.Str(a, "href"); href != "" && !strings.Contains(content, html.EscapeString(href)) {
					content += `<p><a href="` + html.EscapeString(href) + `">` + html.EscapeString(href) + `</a></p>`
				}
			}
		}
	}
	if content == "" && name != "" && ap.Type(note) != "Note" {
		content = "<p>" + html.EscapeString(name) + "</p>"
	}
	st.Content = sanitizeHTML(content)
	st.Spoiler = truncateRunes(plainText(summary), 500)
	st.Sensitive = ap.Bool(note, "sensitive") || st.Spoiler != ""
	st.Emojis = emojisOf(note)
	st.QuoteURI = quoteOf(note)
	st.Poll = nil
	if ap.Type(note) == "Question" {
		p := &Poll{EndTime: ap.ParseTime(ap.Str(note, "endTime"))}
		if c := ap.ParseTime(ap.Str(note, "closed")); !c.IsZero() {
			p.EndTime = c
		}
		opts := ap.Objects(note["oneOf"])
		if len(opts) == 0 {
			opts = ap.Objects(note["anyOf"])
			p.Multiple = true
		}
		for _, o := range opts {
			votes := 0
			if r, ok := o["replies"].(obj); ok {
				if n, ok := r["totalItems"].(float64); ok {
					votes = int(n)
				}
			}
			p.Options = append(p.Options, PollOption{Name: truncateRunes(ap.Str(o, "name"), 200), Votes: votes})
		}
		if len(p.Options) > 0 {
			st.Poll = p
		}
	}
}

// replaceNoteExtras (re)writes mentions, tags and attachments of a remote
// status. It returns the local accounts that are mentioned.
func (app *App) replaceNoteExtras(ctx context.Context, st *Status, note obj) map[string]bool {
	app.db.Exec(`DELETE FROM mentions WHERE status_id=?`, st.ID)
	app.db.Exec(`DELETE FROM status_tags WHERE status_id=?`, st.ID)
	app.db.Exec(`DELETE FROM attachments WHERE status_id=?`, st.ID)
	local := map[string]bool{}
	for _, t := range ap.Objects(note["tag"]) {
		switch ap.Type(t) {
		case "Mention":
			href := ap.Str(t, "href")
			acc, err := app.AccountByURI(href)
			if err != nil {
				continue
			}
			app.db.Exec(`INSERT OR IGNORE INTO mentions (status_id, account_id) VALUES (?,?)`, st.ID, acc.ID)
			if acc.IsLocal() && acc.ActorType != "Application" {
				local[acc.ID] = true
			}
		case "Hashtag":
			name := strings.ToLower(strings.TrimPrefix(ap.Str(t, "name"), "#"))
			if name != "" && len(name) < 100 {
				app.db.Exec(`INSERT OR IGNORE INTO status_tags (status_id, tag) VALUES (?,?)`, st.ID, name)
			}
		}
	}
	// people addressed directly (DMs without a Mention tag)
	for _, r := range ap.Recipients(note) {
		if app.isLocalIRI(r) {
			if acc, err := app.localTarget(r); err == nil {
				app.db.Exec(`INSERT OR IGNORE INTO mentions (status_id, account_id) VALUES (?,?)`, st.ID, acc.ID)
				local[acc.ID] = true
			}
		}
	}
	atts := ap.Objects(note["attachment"])
	hasMedia := false
	for _, a := range atts {
		if ap.Type(a) != "Link" || strings.Contains(ap.Str(a, "mediaType"), "/") && !strings.HasPrefix(ap.Str(a, "mediaType"), "text/") {
			hasMedia = true
		}
	}
	if !hasMedia {
		switch ap.Type(note) {
		case "Video", "Page", "Article", "Event", "Audio":
			// show the thumbnail/preview image of videos and link posts
			if img := bestImage(note); img != nil {
				atts = append(atts, img)
			}
		}
	}
	for i, a := range atts {
		if i >= 8 {
			break
		}
		switch ap.Type(a) {
		case "Document", "Image", "Video", "Audio":
		case "Link":
			if mt := ap.Str(a, "mediaType"); !strings.HasPrefix(mt, "image/") && !strings.HasPrefix(mt, "video/") && !strings.HasPrefix(mt, "audio/") {
				continue
			}
		default:
			continue
		}
		u := ap.URL(a["url"])
		if u == "" {
			u = ap.Str(a, "href")
		}
		if !strings.HasPrefix(u, "https://") && !(app.cfg.Insecure && strings.HasPrefix(u, "http://")) {
			continue
		}
		mt := ap.Str(a, "mediaType")
		if mt == "" {
			if l := ap.Object(a["url"]); l != nil {
				mt = ap.Str(l, "mediaType")
			}
		}
		if mt == "" {
			mt = mime.TypeByExtension(strings.ToLower(path.Ext(strings.SplitN(u, "?", 2)[0])))
		}
		if mt == "" {
			switch ap.Type(a) {
			case "Image":
				mt = "image/jpeg"
			case "Video":
				mt = "video/mp4"
			case "Audio":
				mt = "audio/mpeg"
			}
		}
		w, _ := a["width"].(float64)
		h, _ := a["height"].(float64)
		// Mastodon sends a video's or audio file's thumbnail as its icon
		poster := ""
		if icon := ap.Object(a["icon"]); icon != nil && !strings.HasPrefix(mt, "image/") {
			if p := ap.URL(icon["url"]); strings.HasPrefix(p, "https://") || (app.cfg.Insecure && strings.HasPrefix(p, "http://")) {
				poster = p
			}
		}
		var fx, fy float64
		if fp, ok := a["focalPoint"].([]any); ok && len(fp) == 2 {
			x, _ := fp[0].(float64)
			y, _ := fp[1].(float64)
			fx, fy = max(-1, min(1, x)), max(-1, min(1, y))
		}
		app.db.Exec(`INSERT INTO attachments (id,status_id,account_id,url,media_type,description,width,height,position,created_at,preview_url,focus_x,focus_y) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			ulid.New(), st.ID, st.AccountID, u, mt, truncateRunes(ap.Str(a, "name"), 1500), int(w), int(h), i, time.Now().Unix(), poster, fx, fy)
	}
	return local
}

func (app *App) resolveStatus(ctx context.Context, iri string) (*Status, error) {
	return app.resolveStatusDepth(ctx, iri, 0)
}

func (app *App) resolveStatusDepth(ctx context.Context, iri string, depth int) (*Status, error) {
	if st, err := app.StatusByURI(iri); err == nil {
		return st, nil
	}
	if app.isLocalIRI(iri) {
		return nil, errNotFound
	}
	note, err := app.fetchAP(ctx, iri)
	if err != nil {
		return nil, err
	}
	if st, err := app.StatusByURI(ap.Str(note, "id")); err == nil {
		return st, nil
	}
	return app.storeFetchedNote(ctx, note, depth)
}

func (app *App) storeFetchedNote(ctx context.Context, note obj, depth int) (*Status, error) {
	if !noteTypes[ap.Type(note)] {
		return nil, errors.New("not a post")
	}
	a, _ := attributedTo(note, "")
	author, err := app.getAccount(ctx, a)
	if err != nil {
		return nil, err
	}
	if !ap.SameHost(ap.Str(note, "id"), author.URI) {
		return nil, errors.New("post and author on different hosts")
	}
	if author.Suspended {
		return nil, errBlocked
	}
	return app.storeNote(ctx, author, note, depth)
}

var (
	backfillMu   sync.Mutex
	backfillSeen = map[string]time.Time{}
)

// backfillOutbox pulls the most recent public posts of a remote account so
// that a fresh follow or profile view isn't empty.
func (app *App) backfillOutbox(ctx context.Context, acc *Account) {
	if acc.IsLocal() || acc.Outbox == "" {
		return
	}
	backfillMu.Lock()
	if t, ok := backfillSeen[acc.ID]; ok && time.Since(t) < 6*time.Hour {
		backfillMu.Unlock()
		return
	}
	backfillSeen[acc.ID] = time.Now()
	backfillMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	coll, err := app.fetchAP(ctx, acc.Outbox)
	if err != nil || !ap.SameHost(ap.Str(coll, "id"), acc.URI) {
		return
	}
	page := coll
	if _, ok := coll["orderedItems"]; !ok {
		first := coll["first"]
		if m, ok := first.(obj); ok && m["orderedItems"] != nil {
			page = m
		} else if iri := ap.ID(first); iri != "" {
			page, err = app.fetchAP(ctx, iri)
			if err != nil || !ap.SameHost(ap.Str(page, "id"), acc.URI) {
				return
			}
		}
	}
	items, _ := page["orderedItems"].([]any)
	fetches := 0
	for i, it := range items {
		if i >= 20 {
			break
		}
		act, ok := it.(obj)
		if !ok || ap.Type(act) != "Create" {
			continue
		}
		note, ok := act["object"].(obj)
		if !ok {
			// some servers (GoToSocial) only list IRIs; dereference a few public ones
			iri := ap.ID(act["object"])
			if v := ap.Visibility(act, acc.FollowersURI); (v != "public" && v != "unlisted") || !ap.SameHost(iri, acc.URI) || fetches >= 10 {
				continue
			}
			if _, err := app.StatusByURI(iri); err == nil {
				continue
			}
			fetches++
			if note, err = app.fetchAP(ctx, iri); err != nil {
				continue
			}
		}
		if !noteTypes[ap.Type(note)] || !attributedToIncludes(note, acc.URI) || !ap.SameHost(ap.Str(note, "id"), acc.URI) {
			continue
		}
		if v := ap.Visibility(note, acc.FollowersURI); v != "public" && v != "unlisted" {
			continue
		}
		app.storeNote(ctx, acc, note, 5)
	}
}

// resolveURL looks up an arbitrary URL (profile or post) for search.
func (app *App) resolveURL(ctx context.Context, raw string) (*Account, *Status, error) {
	if app.isLocalIRI(raw) {
		if st, err := app.StatusByURI(raw); err == nil {
			return nil, st, nil
		}
		if st, err := app.statusWhere(`s.url=?`, raw); err == nil {
			return nil, st, nil
		}
		if acc, err := app.accountWhere(`a.uri=? OR a.url=?`, raw, raw); err == nil {
			return acc, nil, nil
		}
		return nil, nil, errNotFound
	}
	if st, err := app.statusWhere(`s.uri=? OR s.url=?`, raw, raw); err == nil {
		return nil, st, nil
	}
	if acc, err := app.accountWhere(`a.uri=? OR a.url=?`, raw, raw); err == nil {
		return acc, nil, nil
	}
	m, err := app.fetchAP(ctx, raw)
	if err != nil {
		return nil, nil, err
	}
	if actorTypes[ap.Type(m)] {
		acc, err := app.upsertActor(m)
		return acc, nil, err
	}
	if st, err := app.StatusByURI(ap.Str(m, "id")); err == nil {
		return nil, st, nil
	}
	st, err := app.storeFetchedNote(ctx, m, 0)
	return nil, st, err
}
