package main

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
		for _, im := range objsOf(note[k]) {
			w, _ := im["width"].(float64)
			if urlOf(im["url"]) != "" && w > bestW {
				best, bestW = obj{"type": "Image", "url": urlOf(im["url"]), "mediaType": str(im, "mediaType")}, w
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
	uri := str(note, "id")
	if uri == "" {
		return nil, errors.New("note without id")
	}
	if st, err := app.StatusByURI(uri); err == nil {
		return st, nil
	}
	published := parseTime(str(note, "published"))
	if published.IsZero() || published.After(time.Now()) {
		published = time.Now()
	}
	st := &Status{
		ID:           newIDAt(published),
		URI:          uri,
		URL:          urlOf(note["url"]),
		AccountID:    author.ID,
		InReplyToURI: idOf(note["inReplyTo"]),
		Visibility:   visibilityFrom(note, author.FollowersURI),
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
	content := str(note, "content")
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
	if strings.HasPrefix(str(note, "mediaType"), "text/markdown") && !strings.Contains(content, "</") {
		content = markdownToHTML(content) // PeerTube video descriptions
	}
	name := str(note, "name")
	summary := str(note, "summary")
	longform := false
	switch typeOf(note) {
	case "Article", "Page", "Event", "Video", "Audio", "Image":
		longform = name != ""
		if longform {
			link := urlOf(note["url"])
			if link == "" {
				link = str(note, "id")
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
		for _, a := range objsOf(note["attachment"]) {
			if typeOf(a) == "Link" && !strings.HasPrefix(str(a, "mediaType"), "image/") {
				if href := str(a, "href"); href != "" && !strings.Contains(content, html.EscapeString(href)) {
					content += `<p><a href="` + html.EscapeString(href) + `">` + html.EscapeString(href) + `</a></p>`
				}
			}
		}
	}
	if content == "" && name != "" && typeOf(note) != "Note" {
		content = "<p>" + html.EscapeString(name) + "</p>"
	}
	st.Content = sanitizeHTML(content)
	st.Spoiler = truncateRunes(plainText(summary), 500)
	st.Sensitive = boolOf(note, "sensitive") || st.Spoiler != ""
	st.Emojis = emojisOf(note)
	st.QuoteURI = quoteOf(note)
	st.Poll = nil
	if typeOf(note) == "Question" {
		p := &Poll{EndTime: parseTime(str(note, "endTime"))}
		if c := parseTime(str(note, "closed")); !c.IsZero() {
			p.EndTime = c
		}
		opts := objsOf(note["oneOf"])
		if len(opts) == 0 {
			opts = objsOf(note["anyOf"])
			p.Multiple = true
		}
		for _, o := range opts {
			votes := 0
			if r, ok := o["replies"].(obj); ok {
				if n, ok := r["totalItems"].(float64); ok {
					votes = int(n)
				}
			}
			p.Options = append(p.Options, PollOption{Name: truncateRunes(str(o, "name"), 200), Votes: votes})
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
	for _, t := range objsOf(note["tag"]) {
		switch typeOf(t) {
		case "Mention":
			href := str(t, "href")
			acc, err := app.AccountByURI(href)
			if err != nil {
				continue
			}
			app.db.Exec(`INSERT OR IGNORE INTO mentions (status_id, account_id) VALUES (?,?)`, st.ID, acc.ID)
			if acc.IsLocal() && acc.ActorType != "Application" {
				local[acc.ID] = true
			}
		case "Hashtag":
			name := strings.ToLower(strings.TrimPrefix(str(t, "name"), "#"))
			if name != "" && len(name) < 100 {
				app.db.Exec(`INSERT OR IGNORE INTO status_tags (status_id, tag) VALUES (?,?)`, st.ID, name)
			}
		}
	}
	// people addressed directly (DMs without a Mention tag)
	for _, r := range recipientsOf(note) {
		if app.isLocalIRI(r) {
			if acc, err := app.localTarget(r); err == nil {
				app.db.Exec(`INSERT OR IGNORE INTO mentions (status_id, account_id) VALUES (?,?)`, st.ID, acc.ID)
				local[acc.ID] = true
			}
		}
	}
	atts := objsOf(note["attachment"])
	hasMedia := false
	for _, a := range atts {
		if typeOf(a) != "Link" || strings.Contains(str(a, "mediaType"), "/") && !strings.HasPrefix(str(a, "mediaType"), "text/") {
			hasMedia = true
		}
	}
	if !hasMedia {
		switch typeOf(note) {
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
		switch typeOf(a) {
		case "Document", "Image", "Video", "Audio":
		case "Link":
			if mt := str(a, "mediaType"); !strings.HasPrefix(mt, "image/") && !strings.HasPrefix(mt, "video/") && !strings.HasPrefix(mt, "audio/") {
				continue
			}
		default:
			continue
		}
		u := urlOf(a["url"])
		if u == "" {
			u = str(a, "href")
		}
		if !strings.HasPrefix(u, "https://") && !(app.cfg.Insecure && strings.HasPrefix(u, "http://")) {
			continue
		}
		mt := str(a, "mediaType")
		if mt == "" {
			if l := objOf(a["url"]); l != nil {
				mt = str(l, "mediaType")
			}
		}
		if mt == "" {
			mt = mime.TypeByExtension(strings.ToLower(path.Ext(strings.SplitN(u, "?", 2)[0])))
		}
		if mt == "" {
			switch typeOf(a) {
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
		app.db.Exec(`INSERT INTO attachments (id,status_id,account_id,url,media_type,description,width,height,position,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			newID(), st.ID, st.AccountID, u, mt, truncateRunes(str(a, "name"), 1500), int(w), int(h), i, time.Now().Unix())
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
	if st, err := app.StatusByURI(str(note, "id")); err == nil {
		return st, nil
	}
	return app.storeFetchedNote(ctx, note, depth)
}

func (app *App) storeFetchedNote(ctx context.Context, note obj, depth int) (*Status, error) {
	if !noteTypes[typeOf(note)] {
		return nil, errors.New("not a post")
	}
	a, _ := attributedTo(note, "")
	author, err := app.getAccount(ctx, a)
	if err != nil {
		return nil, err
	}
	if !sameHost(str(note, "id"), author.URI) {
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
	if err != nil || !sameHost(str(coll, "id"), acc.URI) {
		return
	}
	page := coll
	if _, ok := coll["orderedItems"]; !ok {
		first := coll["first"]
		if m, ok := first.(obj); ok && m["orderedItems"] != nil {
			page = m
		} else if iri := idOf(first); iri != "" {
			page, err = app.fetchAP(ctx, iri)
			if err != nil || !sameHost(str(page, "id"), acc.URI) {
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
		if !ok || typeOf(act) != "Create" {
			continue
		}
		note, ok := act["object"].(obj)
		if !ok {
			// some servers (GoToSocial) only list IRIs; dereference a few public ones
			iri := idOf(act["object"])
			if v := visibilityFrom(act, acc.FollowersURI); (v != "public" && v != "unlisted") || !sameHost(iri, acc.URI) || fetches >= 10 {
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
		if !noteTypes[typeOf(note)] || !attributedToIncludes(note, acc.URI) || !sameHost(str(note, "id"), acc.URI) {
			continue
		}
		if v := visibilityFrom(note, acc.FollowersURI); v != "public" && v != "unlisted" {
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
	if actorTypes[typeOf(m)] {
		acc, err := app.upsertActor(m)
		return acc, nil, err
	}
	if st, err := app.StatusByURI(str(m, "id")); err == nil {
		return nil, st, nil
	}
	st, err := app.storeFetchedNote(ctx, m, 0)
	return nil, st, err
}
