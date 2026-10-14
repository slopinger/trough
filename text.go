package main

import (
	"context"
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
)

// ---- sanitizing remote HTML ----

var htmlPolicy = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "br", "span", "strong", "b", "em", "i", "u", "s", "del", "code", "pre",
		"blockquote", "ul", "ol", "li", "sup", "sub", "small", "h1", "h2", "h3", "h4", "h5", "h6", "ruby", "rt", "rp")
	p.AllowAttrs("href").OnElements("a")
	p.AllowURLSchemes("http", "https")
	p.RequireParseableURLs(true)
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^[a-z\- ]{1,80}$`)).OnElements("a", "span")
	p.AllowAttrs("start").Matching(bluemonday.Integer).OnElements("ol")
	p.AllowAttrs("value").Matching(bluemonday.Integer).OnElements("li")
	return p
}()

var stripPolicy = bluemonday.StrictPolicy()

func sanitizeHTML(s string) string {
	return strings.TrimSpace(htmlPolicy.Sanitize(s))
}

// plainText strips all markup (used for previews, titles, content warnings).
func plainText(s string) string {
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "</p>", "</p>\n\n")
	return strings.TrimSpace(html.UnescapeString(stripPolicy.Sanitize(s)))
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// ---- formatting local posts ----

type formatted struct {
	HTML     string
	Mentions []*Account
	Tags     []string
}

var tokenRe = regexp.MustCompile(`https?://[^\s<>"]+|@[A-Za-z0-9_][A-Za-z0-9_.\-]*(?:@[A-Za-z0-9][A-Za-z0-9.\-]*[A-Za-z0-9](?::[0-9]+)?)?|#[\p{L}\p{N}_]+`)

func wordChar(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// formatPost turns plain text into HTML: paragraphs, line breaks, links,
// @mentions (resolved via WebFinger) and #hashtags.
func (app *App) formatPost(ctx context.Context, text string) formatted {
	var f formatted
	seenM := map[string]bool{}
	seenT := map[string]bool{}
	text = strings.ReplaceAll(strings.TrimSpace(text), "\r\n", "\n")
	var out strings.Builder
	paras := regexp.MustCompile(`\n{2,}`).Split(text, -1)
	for _, para := range paras {
		if strings.TrimSpace(para) == "" {
			continue
		}
		out.WriteString("<p>")
		lines := strings.Split(para, "\n")
		for li, line := range lines {
			if li > 0 {
				out.WriteString("<br>")
			}
			last := 0
			for _, loc := range tokenRe.FindAllStringIndex(line, -1) {
				start, end := loc[0], loc[1]
				tok := line[start:end]
				if start > 0 {
					prev, _ := utf8.DecodeLastRuneInString(line[:start])
					if tok[0] != 'h' && (wordChar(prev) || prev == '/' || prev == '&') {
						continue
					}
				}
				var repl string
				switch tok[0] {
				case 'h':
					// trim trailing punctuation, keeping balanced parentheses
					for len(tok) > 0 && strings.ContainsRune(".,;:!?'\")]", rune(tok[len(tok)-1])) {
						if tok[len(tok)-1] == ')' && strings.Count(tok, "(") >= strings.Count(tok, ")") {
							break
						}
						tok = tok[:len(tok)-1]
					}
					end = start + len(tok)
					repl = linkHTML(tok)
				case '@':
					handle := strings.TrimRight(tok[1:], ".-")
					end = start + 1 + len(handle)
					user, domain, _ := strings.Cut(handle, "@")
					acc, err := app.resolveAcct(ctx, user, domain)
					if err != nil || acc.Suspended {
						continue
					}
					repl = mentionHTML(acc)
					if !seenM[acc.ID] {
						seenM[acc.ID] = true
						f.Mentions = append(f.Mentions, acc)
					}
				case '#':
					tag := tok[1:]
					if !strings.ContainsFunc(tag, unicode.IsLetter) {
						continue
					}
					repl = `<a href="` + app.cfg.BaseURL() + `/tags/` + html.EscapeString(strings.ToLower(tag)) +
						`" class="mention hashtag" rel="tag">#<span>` + html.EscapeString(tag) + `</span></a>`
					if lt := strings.ToLower(tag); !seenT[lt] {
						seenT[lt] = true
						f.Tags = append(f.Tags, lt)
					}
				}
				out.WriteString(html.EscapeString(line[last:start]))
				out.WriteString(repl)
				last = end
			}
			out.WriteString(html.EscapeString(line[last:]))
		}
		out.WriteString("</p>")
	}
	f.HTML = out.String()
	return f
}

func linkHTML(u string) string {
	disp := u
	scheme := ""
	if i := strings.Index(disp, "://"); i >= 0 {
		scheme, disp = disp[:i+3], disp[i+3:]
	}
	rest := ""
	if utf8.RuneCountInString(disp) > 30 {
		r := []rune(disp)
		disp, rest = string(r[:30]), string(r[30:])
	}
	cls := ""
	if rest != "" {
		cls = "ellipsis"
	}
	return `<a href="` + html.EscapeString(u) + `" rel="nofollow noopener noreferrer" target="_blank">` +
		`<span class="invisible">` + html.EscapeString(scheme) + `</span><span class="` + cls + `">` + html.EscapeString(disp) +
		`</span><span class="invisible">` + html.EscapeString(rest) + `</span></a>`
}

func mentionHTML(acc *Account) string {
	href := acc.URL
	if href == "" {
		href = acc.URI
	}
	return `<span class="h-card"><a href="` + html.EscapeString(href) + `" class="u-url mention">@<span>` +
		html.EscapeString(acc.Username) + `</span></a></span>`
}

// formatBio formats profile text: like a post but without creating mentions
// records.
func (app *App) formatBio(ctx context.Context, text string) string {
	return app.formatPost(ctx, text).HTML
}
