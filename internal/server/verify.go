package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/slopinger/trough/internal/ap"
	xhtml "golang.org/x/net/html"
)

// Profile link verification, like Mastodon's: a profile field whose value is
// a link gets a check mark once the linked page links back to the profile
// with rel="me". Each server checks the links itself, so the result is kept
// in the stored fields and not federated.

var verifySem = make(chan struct{}, 2)

// fieldLink returns the URL a field value consists of: one http(s) link and
// no other text. Anything else can't be verified.
func fieldLink(value string) string {
	z := xhtml.NewTokenizer(strings.NewReader(value))
	href, links, depth := "", 0, 0
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			if links != 1 || (!strings.HasPrefix(href, "https://") && !strings.HasPrefix(href, "http://")) {
				return ""
			}
			return href
		case xhtml.StartTagToken:
			tok := z.Token()
			if tok.Data == "a" {
				links++
				depth++
				for _, a := range tok.Attr {
					if a.Key == "href" {
						href = strings.TrimSpace(a.Val)
					}
				}
			}
		case xhtml.EndTagToken:
			if z.Token().Data == "a" {
				depth--
			}
		case xhtml.TextToken:
			if depth == 0 && strings.TrimSpace(string(z.Text())) != "" {
				return ""
			}
		}
	}
}

// keepVerified copies the verification of fields that didn't change (same
// name and value) from old to fields; changed fields lose it.
func keepVerified(old, fields []Field) {
	for i := range fields {
		fields[i].VerifiedAt = 0
		for _, o := range old {
			if o.Name == fields[i].Name && o.Value == fields[i].Value {
				fields[i].VerifiedAt = o.VerifiedAt
			}
		}
	}
}

// verifyLinksLater checks acc's unverified link fields in the background.
// With onlyChanged, fields whose value is also in old are skipped: remote
// actors are refetched often and their sites shouldn't be fetched each time.
func (app *App) verifyLinksLater(acc *Account, old []Field, onlyChanged bool) {
	var links []string
	for _, f := range acc.Fields {
		link := fieldLink(f.Value)
		if link == "" || f.VerifiedAt != 0 || slices.Contains(links, link) {
			continue
		}
		if onlyChanged && slices.ContainsFunc(old, func(o Field) bool { return o.Value == f.Value }) {
			continue
		}
		links = append(links, link)
	}
	if len(links) == 0 || acc.ID == "" {
		return
	}
	targets := []string{acc.URI}
	// A remote actor picks its own url, so only trust one on its own host: a
	// page linking to someone else's profile mustn't verify this account.
	if acc.IsLocal() || ap.SameHost(acc.URL, acc.URI) {
		targets = append(targets, acc.URL)
	}
	id := acc.ID
	go func() {
		verifySem <- struct{}{}
		defer func() { <-verifySem }()
		var ok []string
		for _, link := range links {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			if app.linksBack(ctx, link, targets) {
				ok = append(ok, link)
			}
			cancel()
		}
		if len(ok) > 0 {
			app.markVerified(id, ok)
		}
	}()
}

// markVerified sets verified_at on the account's fields linking to one of
// links, on the current stored fields so a concurrent edit isn't undone.
func (app *App) markVerified(id string, links []string) {
	acc, err := app.AccountByID(id)
	if err != nil {
		return
	}
	now, changed := time.Now().Unix(), false
	for i, f := range acc.Fields {
		if f.VerifiedAt == 0 && slices.Contains(links, fieldLink(f.Value)) {
			acc.Fields[i].VerifiedAt, changed = now, true
		}
	}
	if changed {
		b, _ := json.Marshal(acc.Fields)
		app.db.Exec(`UPDATE accounts SET fields=? WHERE id=?`, string(b), id)
	}
}

// linksBack fetches link and reports whether the page has an <a> or <link>
// with rel="me" pointing to one of targets.
func (app *App) linksBack(ctx context.Context, link string, targets []string) bool {
	u, err := app.checkRemoteURL(link)
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", app.userAgent()+" link-verification")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := app.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "html") {
		return false
	}
	want := map[string]bool{}
	for _, t := range targets {
		if t != "" {
			want[normalizeMeURL(t)] = true
		}
	}
	found, _ := relMeLinks(io.LimitReader(resp.Body, 1<<20), resp.Request.URL)
	for _, h := range found {
		if want[normalizeMeURL(h)] {
			return true
		}
	}
	return false
}

// relMeLinks lists the absolute hrefs of <a rel="me"> and <link rel="me">.
func relMeLinks(r io.Reader, base *url.URL) ([]string, error) {
	var out []string
	z := xhtml.NewTokenizer(r)
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			if errors.Is(z.Err(), io.EOF) {
				return out, nil
			}
			return out, z.Err()
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			tok := z.Token()
			if tok.Data != "a" && tok.Data != "link" {
				continue
			}
			var href string
			me := false
			for _, a := range tok.Attr {
				switch strings.ToLower(a.Key) {
				case "href":
					href = strings.TrimSpace(a.Val)
				case "rel":
					me = slices.Contains(strings.Fields(strings.ToLower(a.Val)), "me")
				}
			}
			if me && href != "" {
				if h, err := base.Parse(href); err == nil {
					out = append(out, h.String())
				}
			}
		}
	}
}

// normalizeMeURL compares profile URLs loosely: scheme and host case, a
// trailing slash and fragments don't matter.
func normalizeMeURL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return s
	}
	u.Scheme, u.Host, u.Fragment = strings.ToLower(u.Scheme), strings.ToLower(u.Host), ""
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String()
}
