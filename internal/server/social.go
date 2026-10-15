package server

import (
	"context"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"

	"git.freifick.net/slop/trough/internal/ulid"
)

// ---- emoji reactions ----

// Reactions come as Misskey-style Likes with content, or as EmojiReact
// (Pleroma, Akkoma, Misskey forks). We send EmojiReact.

var reactionChoices = []string{"👍", "❤️", "😂", "😮", "😢", "🎉", "🔥", "🤓"}

func validReaction(e string) bool {
	return e != "" && utf8.RuneCountInString(e) <= 8 && !strings.ContainsAny(e, "<>\"' \t\n")
}

// storeReaction records an incoming reaction; content is the emoji or :shortcode:.
func (app *App) storeReaction(actor *Account, st *Status, act obj, content string) bool {
	content = strings.TrimSpace(content)
	if content == "" || utf8.RuneCountInString(content) > 64 {
		return false
	}
	emojiURL := ""
	if strings.HasPrefix(content, ":") && strings.HasSuffix(content, ":") {
		for _, e := range emojisOf(act) {
			if ":"+e.Shortcode+":" == content {
				emojiURL = e.URL
			}
		}
		if emojiURL == "" {
			return false // custom emoji without an image: nothing to show
		}
	} else if !validReaction(content) {
		return false
	}
	res, err := app.db.Exec(`INSERT OR IGNORE INTO reactions (status_id, account_id, emoji, emoji_url, uri, created_at) VALUES (?,?,?,?,?,?)`,
		st.ID, actor.ID, content, emojiURL, str(act, "id"), time.Now().Unix())
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

func (app *App) inEmojiReact(actor *Account, act obj) error {
	st, err := app.StatusByURI(idOf(act["object"]))
	if err != nil {
		return nil
	}
	content := str(act, "content")
	if content == "" {
		content = str(act, "_misskey_reaction")
	}
	if app.storeReaction(actor, st, act, content) && st.Local {
		app.notify(st.AccountID, "reaction", actor.ID, st.ID)
	}
	return nil
}

func (app *App) React(me *Account, st *Status, emoji string) error {
	if st.IsBoost() {
		st = st.BoostOf
	}
	if !validReaction(emoji) {
		return errors.New("invalid reaction")
	}
	id := ulid.New()
	uri := me.URI + "#reactions/" + id
	res, err := app.db.Exec(`INSERT OR IGNORE INTO reactions (status_id, account_id, emoji, uri, created_at) VALUES (?,?,?,?,?)`,
		st.ID, me.ID, emoji, uri, time.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if st.Account.IsLocal() {
		app.notify(st.AccountID, "reaction", me.ID, st.ID)
	} else {
		app.enqueue(me, []string{st.Account.DeliveryInbox()}, reactActivity(uri, me, st, emoji))
	}
	return nil
}

func reactActivity(uri string, me *Account, st *Status, emoji string) obj {
	return withContext(obj{"id": uri, "type": "EmojiReact", "actor": me.URI, "object": st.URI, "content": emoji,
		"to": []any{st.Account.URI}})
}

func (app *App) Unreact(me *Account, st *Status, emoji string) error {
	if st.IsBoost() {
		st = st.BoostOf
	}
	var uri string
	if err := app.db.QueryRow(`SELECT uri FROM reactions WHERE status_id=? AND account_id=? AND emoji=?`, st.ID, me.ID, emoji).Scan(&uri); err != nil {
		return nil
	}
	app.db.Exec(`DELETE FROM reactions WHERE status_id=? AND account_id=? AND emoji=?`, st.ID, me.ID, emoji)
	app.db.Exec(`DELETE FROM notifications WHERE type='reaction' AND from_account_id=? AND status_id=?`, me.ID, st.ID)
	if !st.Account.IsLocal() {
		app.enqueue(me, []string{st.Account.DeliveryInbox()}, undoActivity(me, reactActivity(uri, me, st, emoji)))
	}
	return nil
}

func (app *App) reactionsFor(v *Account, ids []string) map[string][]ReactionCount {
	out := map[string][]ReactionCount{}
	if len(ids) == 0 {
		return out
	}
	q, args := inClause(`SELECT status_id, emoji, MAX(emoji_url), COUNT(*), MAX(account_id = ?) FROM reactions
		WHERE status_id IN (%s) GROUP BY status_id, emoji ORDER BY COUNT(*) DESC, MIN(created_at)`, ids)
	rows, err := app.db.Query(q, append([]any{viewerID(v)}, args...)...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var r ReactionCount
		rows.Scan(&id, &r.Emoji, &r.URL, &r.Count, &r.Mine)
		if len(out[id]) < 20 {
			out[id] = append(out[id], r)
		}
	}
	return out
}

// ---- quote posts ----

// quoteOf finds the quoted post of an object in the various ways servers
// say it: FEP-044f "quote", Misskey/Fedibird quoteUrl/quoteUri/_misskey_quote,
// or a FEP-e232 object link tag.
func quoteOf(note obj) string {
	for _, k := range []string{"quote", "quoteUri", "quoteUrl", "_misskey_quote"} {
		if v := idOf(note[k]); strings.HasPrefix(v, "http") {
			return v
		}
	}
	for _, t := range objsOf(note["tag"]) {
		mt := str(t, "mediaType")
		if typeOf(t) == "Link" && (strings.Contains(mt, "activity+json") || strings.Contains(mt, "ld+json")) {
			if h := str(t, "href"); strings.HasPrefix(h, "http") {
				return h
			}
		}
	}
	return ""
}

func (app *App) quoteFields(st *Status, m obj) {
	if st.QuoteURI == "" {
		return
	}
	m["quote"], m["quoteUri"], m["quoteUrl"], m["_misskey_quote"] = st.QuoteURI, st.QuoteURI, st.QuoteURI, st.QuoteURI
	if st.QuoteAuth != "" {
		m["quoteAuthorization"] = st.QuoteAuth
	}
	tags, _ := m["tag"].([]any)
	m["tag"] = append(tags, obj{"type": "Link", "mediaType": ldMimeType, "href": st.QuoteURI, "name": "RE: " + st.QuoteURI})
}

// hydrateQuotes attaches quoted posts (one level deep) that the viewer may see.
func (app *App) hydrateQuotes(v *Account, sts []*Status) {
	var uris []string
	for _, s := range sts {
		for _, x := range []*Status{s, s.BoostOf} {
			if x != nil && x.QuoteURI != "" {
				uris = append(uris, x.QuoteURI)
			}
		}
	}
	if len(uris) == 0 {
		return
	}
	q, args := inClause(`s.uri IN (%s)`, uniq(uris))
	quoted, err := app.queryStatusesNoQuotes(v, q, args, "", 0, false)
	if err != nil {
		return
	}
	byURI := map[string]*Status{}
	for _, qs := range quoted {
		if !qs.IsBoost() {
			byURI[qs.URI] = qs
		}
	}
	for _, s := range sts {
		for _, x := range []*Status{s, s.BoostOf} {
			if x != nil && x.QuoteURI != "" && x.QuoteState == "" {
				x.Quote = byURI[x.QuoteURI]
			}
		}
	}
}

// ---- link previews ----

var cardSem = make(chan struct{}, 4)

// firstLink returns the first link in post HTML that isn't a mention,
// hashtag or link to the quoted post.
func firstLink(content, skip string) string {
	z := xhtml.NewTokenizer(strings.NewReader(content))
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			return ""
		case xhtml.StartTagToken:
			tok := z.Token()
			if tok.Data != "a" {
				continue
			}
			var href, class string
			for _, a := range tok.Attr {
				switch a.Key {
				case "href":
					href = a.Val
				case "class":
					class = a.Val
				}
			}
			if strings.Contains(class, "mention") || strings.Contains(class, "hashtag") || href == skip {
				continue
			}
			if strings.HasPrefix(href, "https://") || strings.HasPrefix(href, "http://") {
				return href
			}
		}
	}
}

// queueCard fetches a link preview for a post in the background.
func (app *App) queueCard(st *Status) {
	if st.BoostOfID != "" || st.Poll != nil {
		return
	}
	link := firstLink(st.Content, st.QuoteURI)
	if link == "" || app.isLocalIRI(link) {
		return
	}
	go func() {
		cardSem <- struct{}{}
		defer func() { <-cardSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if app.fetchCard(ctx, link) == nil {
			app.db.Exec(`UPDATE statuses SET card_url=? WHERE id=?`, link, st.ID)
		}
	}()
}

func (app *App) fetchCard(ctx context.Context, link string) error {
	var fetched int64
	if app.db.QueryRow(`SELECT fetched_at FROM link_cards WHERE url=?`, link).Scan(&fetched) == nil && time.Since(time.Unix(fetched, 0)) < 7*24*time.Hour {
		return nil
	}
	u, err := app.checkRemoteURL(link)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", app.userAgent()+" link-preview")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := app.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "html") {
		return errors.New("not an html page")
	}
	card := parseCard(io.LimitReader(resp.Body, 512<<10), resp.Request.URL)
	if card.Title == "" {
		return errors.New("page has no title")
	}
	_, err = app.db.Exec(`INSERT INTO link_cards (url,title,description,image,site,fetched_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(url) DO UPDATE SET title=excluded.title, description=excluded.description, image=excluded.image, site=excluded.site, fetched_at=excluded.fetched_at`,
		link, card.Title, card.Description, card.Image, card.Site, time.Now().Unix())
	return err
}

// parseCard reads OpenGraph/Twitter/HTML metadata from the head of a page.
func parseCard(r io.Reader, base *url.URL) LinkCard {
	var c LinkCard
	var title string
	z := xhtml.NewTokenizer(r)
	inTitle := false
	for {
		tt := z.Next()
		switch tt {
		case xhtml.ErrorToken:
			goto done
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			tok := z.Token()
			switch tok.Data {
			case "title":
				inTitle = true
			case "body":
				goto done
			case "meta":
				var prop, content string
				for _, a := range tok.Attr {
					switch strings.ToLower(a.Key) {
					case "property", "name":
						prop = strings.ToLower(a.Val)
					case "content":
						content = strings.TrimSpace(a.Val)
					}
				}
				switch prop {
				case "og:title", "twitter:title":
					if c.Title == "" {
						c.Title = content
					}
				case "og:description", "twitter:description", "description":
					if c.Description == "" {
						c.Description = content
					}
				case "og:image", "twitter:image":
					if c.Image == "" {
						if iu, err := base.Parse(content); err == nil && (iu.Scheme == "https" || iu.Scheme == "http") {
							c.Image = iu.String()
						}
					}
				case "og:site_name":
					c.Site = content
				}
			}
		case xhtml.TextToken:
			if inTitle && title == "" {
				title = strings.TrimSpace(string(z.Text()))
			}
		case xhtml.EndTagToken:
			if z.Token().Data == "title" {
				inTitle = false
			}
		}
	}
done:
	if c.Title == "" {
		c.Title = title
	}
	c.Title = truncateRunes(html.UnescapeString(c.Title), 200)
	c.Description = truncateRunes(html.UnescapeString(c.Description), 300)
	c.Site = truncateRunes(html.UnescapeString(c.Site), 100)
	return c
}

func (app *App) cardsFor(sts []*Status) {
	var urls []string
	for _, s := range sts {
		if s.CardURL != "" {
			urls = append(urls, s.CardURL)
		}
	}
	if len(urls) == 0 {
		return
	}
	q, args := inClause(`SELECT url, title, description, image, site FROM link_cards WHERE url IN (%s)`, uniq(urls))
	rows, err := app.db.Query(q, args...)
	if err != nil {
		return
	}
	cards := map[string]*LinkCard{}
	for rows.Next() {
		c := &LinkCard{}
		rows.Scan(&c.URL, &c.Title, &c.Description, &c.Image, &c.Site)
		cards[c.URL] = c
	}
	rows.Close()
	for _, s := range sts {
		if s.CardURL != "" {
			s.Card = cards[s.CardURL]
		}
	}
}

// notifyEdited tells local users that st, which they boosted or quoted,
// was edited (Mastodon's "update" and "quoted_update" notifications).
// For quotes the notification carries their quoting post.
func (app *App) notifyEdited(st *Status) {
	type note struct{ to, typ, status string }
	var list []note
	rows, err := app.db.Query(`SELECT s.account_id, 'update', ? FROM statuses s JOIN users u ON u.account_id=s.account_id WHERE s.boost_of_id=?
		UNION SELECT s.account_id, 'quoted_update', s.id FROM statuses s JOIN users u ON u.account_id=s.account_id
		WHERE s.quote_uri=? AND s.quote_state='' AND s.boost_of_id=''`, st.ID, st.ID, st.URI)
	if err != nil {
		return
	}
	for rows.Next() {
		var n note
		rows.Scan(&n.to, &n.typ, &n.status)
		list = append(list, n)
	}
	rows.Close()
	for _, n := range list {
		// a later edit notifies again
		app.db.Exec(`DELETE FROM notifications WHERE account_id=? AND type=? AND from_account_id=? AND status_id=?`, n.to, n.typ, st.AccountID, n.status)
		app.notify(n.to, n.typ, st.AccountID, n.status)
	}
}

// ---- featured accounts ("endorsements") ----

// Endorse features target on my profile.
func (app *App) Endorse(me, target *Account) error {
	if me.ID == target.ID {
		return errors.New("you can't feature yourself")
	}
	if app.countInt(`SELECT COUNT(*) FROM account_pins WHERE account_id=?`, me.ID) >= 80 {
		return errors.New("you can feature at most 80 accounts")
	}
	_, err := app.db.Exec(`INSERT OR IGNORE INTO account_pins (account_id, target_id, created_at) VALUES (?,?,?)`, me.ID, target.ID, time.Now().UnixNano())
	return err
}

func (app *App) Unendorse(me, target *Account) error {
	_, err := app.db.Exec(`DELETE FROM account_pins WHERE account_id=? AND target_id=?`, me.ID, target.ID)
	return err
}

// endorsements are the accounts accountID features, newest first.
func (app *App) endorsements(accountID string) ([]*Account, error) {
	return app.accountsQuery(`SELECT `+accountCols+` FROM account_pins p JOIN accounts a ON a.id=p.target_id
		WHERE p.account_id=? AND a.suspended=0 ORDER BY p.created_at DESC LIMIT 80`, accountID)
}
