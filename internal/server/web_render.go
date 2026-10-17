package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"

	"git.freifick.net/slop/trough/internal/ap"
	"git.freifick.net/slop/trough/internal/ulid"
	"git.freifick.net/slop/trough/web"
)

var (
	templateFS = web.Templates
	staticFS   = web.Static
)

type templates struct {
	m map[string]*template.Template
}

var staticHashes = map[string]string{}

func init() {
	entries, _ := fs.ReadDir(staticFS, "static")
	for _, e := range entries {
		b, _ := staticFS.ReadFile("static/" + e.Name())
		h := sha256.Sum256(b)
		staticHashes[e.Name()] = hex.EncodeToString(h[:])[:10]
	}
}

func htmlEscape(s string) string { return html.EscapeString(s) }

func loadTemplates(app *App) (*templates, error) {
	base := template.New("").Funcs(app.templateFuncs())
	if _, err := base.ParseFS(templateFS, "templates/layout.html", "templates/partials.html"); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(templateFS, "templates")
	if err != nil {
		return nil, err
	}
	t := &templates{m: map[string]*template.Template{}}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".html")
		if name == "layout" || name == "partials" {
			continue
		}
		c, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := c.ParseFS(templateFS, "templates/"+e.Name()); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		t.m[name] = c
	}
	return t, nil
}

func (t *templates) execute(w io.Writer, name string, p *Page) error {
	tm, ok := t.m[name]
	if !ok {
		return fmt.Errorf("no template %q", name)
	}
	var buf bytes.Buffer
	if err := tm.ExecuteTemplate(&buf, "layout", p); err != nil {
		return err
	}
	_, err := buf.WriteTo(w)
	return err
}

func (app *App) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(b)
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case t.Year() == time.Now().Year():
		return t.UTC().Format("Jan 2")
	}
	return t.UTC().Format("Jan 2, 2006")
}

func (app *App) templateFuncs() template.FuncMap {
	return template.FuncMap{
		"media":    app.mediaURL,
		"static":   func(name string) string { return "/static/" + name + "?v=" + staticHashes[name] },
		"ago":      ago,
		"isotime":  func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"fulltime": func(t time.Time) string { return t.UTC().Format("Mon, 2 Jan 2006 15:04 UTC") },
		"date":     func(t time.Time) string { return t.UTC().Format("January 2006") },
		"day":      func(t time.Time) string { return t.UTC().Format("2 January 2006") },
		"canQuote": func(st *Status, me *Account) bool { return app.quoteApproval(st, me) != "denied" },
		"join":     strings.Join,
		"handle": func(a *Account) string {
			return "@" + a.Username + "@" + app.accountHost(a)
		},
		"name": func(a *Account) template.HTML {
			return app.emojify(html.EscapeString(a.Name()), a.Emojis)
		},
		"content": func(s *Status) template.HTML {
			return app.renderHTML(s.Content, s.Mentions, s.Emojis)
		},
		"spoiler": func(s *Status) template.HTML {
			return app.emojify(html.EscapeString(s.Spoiler), s.Emojis)
		},
		"bio": func(a *Account) template.HTML {
			return app.renderHTML(a.Note, nil, a.Emojis)
		},
		"field": func(a *Account, v string) template.HTML {
			return app.renderHTML(v, nil, a.Emojis)
		},
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
		"add":          func(a, b int) int { return a + b },
		"languages":    func() any { return postLanguages },
		"postLanguage": app.postLanguage,
		"cleanupAges":  func() any { return cleanupAges },
		"oembedLink":   app.oembedLink,
		"hideMedia":    func(p *Page, s *Status) bool { return p.Prefs.hideMedia(s) },
		"clampint":     func(v, hi int) int { return min(v, hi) },
		"percent": func(v int, p *Poll) int {
			total := 0
			for _, o := range p.Options {
				total += o.Votes
			}
			if total == 0 {
				return 0
			}
			return v * 100 / total
		},
		"pollClosed": func(p *Poll) bool { return !p.EndTime.IsZero() && p.EndTime.Before(time.Now()) },
		"until": func(t time.Time) string {
			d := time.Until(t)
			switch {
			case d < time.Minute:
				return "in a moment"
			case d < time.Hour:
				return fmt.Sprintf("in %d min", int(d.Minutes()))
			case d < 48*time.Hour:
				return fmt.Sprintf("in %dh", int(d.Hours()))
			}
			return fmt.Sprintf("in %d days", int(d.Hours()/24))
		},
		"visIcon": func(v string) string {
			return map[string]string{"public": "🌍", "unlisted": "🔓", "private": "🔒", "direct": "✉️"}[v]
		},
		"visLabel": func(v string) string {
			return map[string]string{"public": "Public", "unlisted": "Unlisted", "private": "Followers only", "direct": "Direct"}[v]
		},
		"hostOf":     ap.Host,
		"sparkline":  sparkline,
		"convMuted":  func(st *Status, me *Account) bool { return app.conversationMuted(me, st.ID) },
		"tagUses":    tagUses,
		"remoteLink": func(s *Status) string { return s.URL },
		"isImage":    func(at *Attachment) bool { return at.Kind() == "image" },
		"localPath": func(u string) string {
			return strings.TrimPrefix(u, app.cfg.BaseURL())
		},
		"plain":           func(s string) string { return truncateRunes(plainText(s), 200) },
		"maxChars":        func() int { return maxStatusChars },
		"aiModel":         app.aiModel,
		"reactionChoices": func() []string { return reactionChoices },
		"versionHTML": func(s *Status, content string) template.HTML {
			return app.renderHTML(content, s.Mentions, s.Emojis)
		},
		"scopeList": func(s string) []string {
			desc := map[string]string{"read": "read your timelines, posts, notifications and account data", "write": "post, edit, delete, like and boost on your behalf, and change your profile",
				"follow": "follow, unfollow, block and mute accounts", "push": "receive push notifications", "profile": "see your profile",
				"admin:read": "read admin data", "admin:write": "perform admin actions"}
			var out []string
			for sc := range parseScopes(s) {
				d := desc[sc]
				if d == "" {
					if base := strings.SplitN(sc, ":", 2)[0]; desc[base] != "" {
						d = desc[base] + " (" + sc + ")"
					} else {
						d = sc
					}
				}
				out = append(out, d)
			}
			sort.Strings(out)
			return out
		},
		"sinceID": func(sts []*Status) string {
			if len(sts) > 0 {
				return sts[0].ID
			}
			return ulid.New()
		},
		"setting": app.getSetting,
		"actions": func(v ...string) [][]string {
			var out [][]string
			for i := 0; i+3 <= len(v); i += 3 {
				out = append(out, v[i:i+3])
			}
			return out
		},
		"muteChoices": func() [][2]string {
			return [][2]string{{"0", "Mute indefinitely"}, {"3600", "Mute for 1 hour"}, {"86400", "Mute for 1 day"}, {"604800", "Mute for 7 days"}}
		},
		"fieldslots": func(a *Account) []Field {
			out := append([]Field{}, a.Fields...)
			for len(out) < 4 {
				out = append(out, Field{})
			}
			return out[:4]
		},
	}
}

// emojify replaces :shortcode: in already-escaped text with emoji images.
func (app *App) emojify(escaped string, emojis []Emoji) template.HTML {
	if len(emojis) == 0 || !strings.Contains(escaped, ":") {
		return template.HTML(escaped)
	}
	for _, e := range emojis {
		code := ":" + e.Shortcode + ":"
		if !strings.Contains(escaped, code) {
			continue
		}
		src := app.mediaURL(e.URL)
		if src == "" {
			continue
		}
		img := `<img class="emoji" src="` + html.EscapeString(src) + `" alt="` + html.EscapeString(code) + `" title="` + html.EscapeString(code) + `">`
		escaped = strings.ReplaceAll(escaped, code, img)
	}
	return template.HTML(escaped)
}

// renderHTML rewrites stored (already sanitized) post HTML for display:
// mentions and hashtags link to our own pages, other links open in a new
// tab, and custom emoji become images.
func (app *App) renderHTML(content string, mentions []*Account, emojis []Emoji) template.HTML {
	if content == "" {
		return ""
	}
	mentionPaths := map[string]string{}
	for _, m := range mentions {
		if m.URL != "" {
			mentionPaths[m.URL] = m.Path()
		}
		mentionPaths[m.URI] = m.Path()
	}
	linkPaths := map[string]string{}
	for _, l := range app.mentionLinks(content) {
		linkPaths[l.Href] = "/@" + l.Handle
	}
	var out strings.Builder
	z := xhtml.NewTokenizer(strings.NewReader(content))
	for {
		tt := z.Next()
		switch tt {
		case xhtml.ErrorToken:
			if z.Err() != io.EOF {
				log.Printf("render: %v", z.Err())
			}
			return template.HTML(out.String())
		case xhtml.TextToken:
			out.WriteString(string(app.emojify(html.EscapeString(string(z.Text())), emojis)))
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			tok := z.Token()
			if tok.Data == "a" {
				var href, class string
				for _, a := range tok.Attr {
					switch a.Key {
					case "href":
						href = a.Val
					case "class":
						class = a.Val
					}
				}
				local := ""
				if strings.Contains(" "+class+" ", " hashtag ") {
					tag := href
					if i := strings.LastIndex(tag, "/"); i >= 0 {
						tag = tag[i+1:]
					}
					if i := strings.IndexAny(tag, "?#"); i >= 0 {
						tag = tag[:i]
					}
					if tag != "" {
						local = "/tags/" + strings.ToLower(tag)
					}
				} else if p, ok := mentionPaths[href]; ok {
					local = p
				} else if p, ok := linkPaths[href]; ok {
					local = p // mentioned account we haven't fetched (yet)
				} else if strings.HasPrefix(href, app.cfg.BaseURL()+"/") {
					local = strings.TrimPrefix(href, app.cfg.BaseURL())
				}
				var attrs []xhtml.Attribute
				for _, a := range tok.Attr {
					if a.Key == "href" || a.Key == "target" || a.Key == "rel" {
						continue
					}
					attrs = append(attrs, a)
				}
				if local != "" {
					attrs = append(attrs, xhtml.Attribute{Key: "href", Val: local})
				} else {
					attrs = append(attrs, xhtml.Attribute{Key: "href", Val: href},
						xhtml.Attribute{Key: "target", Val: "_blank"},
						xhtml.Attribute{Key: "rel", Val: "nofollow noopener noreferrer"})
				}
				tok.Attr = attrs
			}
			out.WriteString(tok.String())
		default:
			out.WriteString(z.Token().String())
		}
	}
}

type mentionLink struct {
	Href, Handle string // Handle is user@domain, or user for local accounts
}

// mentionLinks finds the @mention links in post HTML (class "mention", not
// hashtags) and derives each handle from the link text and host, so that
// mentions of accounts we have never fetched still work.
func (app *App) mentionLinks(content string) []mentionLink {
	var out []mentionLink
	seen := map[string]bool{}
	z := xhtml.NewTokenizer(strings.NewReader(content))
	var href string
	var text strings.Builder
	inMention := false
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			return out
		case xhtml.StartTagToken:
			tok := z.Token()
			if tok.Data != "a" {
				continue
			}
			var class string
			href, class = "", ""
			for _, a := range tok.Attr {
				switch a.Key {
				case "href":
					href = a.Val
				case "class":
					class = a.Val
				}
			}
			c := " " + class + " "
			inMention = strings.Contains(c, " mention ") && !strings.Contains(c, " hashtag ")
			text.Reset()
		case xhtml.TextToken:
			if inMention {
				text.Write(z.Text())
			}
		case xhtml.EndTagToken:
			if tok := z.Token(); tok.Data == "a" && inMention {
				inMention = false
				if h := app.handleFromLink(href, text.String()); h != "" && !seen[strings.ToLower(h)] {
					seen[strings.ToLower(h)] = true
					out = append(out, mentionLink{Href: href, Handle: h})
				}
			}
		}
	}
}

func (app *App) handleFromLink(href, text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "@") {
		return ""
	}
	t = strings.TrimPrefix(t, "@")
	user, domain, _ := strings.Cut(t, "@")
	if !usernameLikeRe.MatchString(user) {
		return ""
	}
	if domain == "" {
		domain = ap.Host(href)
	}
	domain = strings.ToLower(domain)
	if domain == "" || strings.ContainsAny(domain, " /?#@") {
		return ""
	}
	if domain == app.cfg.Host {
		return user
	}
	return user + "@" + domain
}

var usernameLikeRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]{0,63}$`)
