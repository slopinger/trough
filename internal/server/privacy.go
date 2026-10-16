package server

import (
	"html"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The privacy policy (/privacy-policy, GET /api/v1/instance/privacy_policy,
// Mastodon 4.0): text the admin writes on /admin, or a default describing
// what trough itself stores and shares.

const defaultPrivacyPolicy = `# About this page
{host} runs trough, a small fediverse server. The admins haven't written their own privacy policy yet, so this one describes what the software stores and shares. They are listed on the About page; ask them about anything specific to this server, such as backups and the logs of the web server in front of trough.

# What is stored about you
- Your account: username, display name, bio, profile fields, avatar and header, and a hash of your password (never the password itself). If you turn on two-factor login, its secret and recovery codes.
- What you post: posts, edits, polls, uploaded media and their descriptions. Direct messages are stored like other posts; the admins could read them in the database.
- What you do: whom you follow, block and mute, your likes, boosts, bookmarks, lists, filters, notes on accounts, poll votes, reports and notifications.
- Your settings and logins: preferences, the browsers you are logged in with (their name from the user agent, when they logged in and were last used), the apps you connected and, if you turn on browser notifications, the push address your browser gave.
- trough doesn't save IP addresses. They are kept in memory for a short while to slow down repeated login and signup attempts.

# What other servers get
- Your profile, public and unlisted posts and, unless you hide them, whom you follow and who follows you are public, and other fediverse servers fetch and copy them.
- Followers-only posts and direct messages are delivered to the servers of the people who may see them. Their admins can read them like the admins here can.
- When you edit or delete something, other servers are told; this server can't make sure they follow.
- Profiles, posts and media from other servers are stored or cached here so you can see them.

# Optional features that use other services
- If the admins turned on AI writing help or translation, the text you ask it to write, rewrite or translate (for replies also the thread) is sent to the AI provider they chose, only when you use those buttons.
- Browser notifications are delivered through the push service of your browser's maker.

# Your choices
- You can edit and delete your posts, have old posts deleted automatically, hide whom you follow and who follows you, log out other browsers, download an archive of your data and export follows, lists, blocks, mutes and bookmarks (Settings).
- Deleting your account (Settings) removes your data here and asks other servers to remove their copies. Your username stays reserved so nobody can pretend to be you.`

// privacyPolicy returns the policy text, when it last changed and whether it
// is the default.
func (app *App) privacyPolicy() (string, time.Time, bool) {
	text := app.getSetting("privacy_policy")
	updated, _ := strconv.ParseInt(app.getSetting("privacy_policy_updated"), 10, 64)
	if strings.TrimSpace(text) == "" {
		return strings.ReplaceAll(defaultPrivacyPolicy, "{host}", app.cfg.Host), app.instanceActor().CreatedAt, true
	}
	return text, time.Unix(updated, 0), false
}

// policyHTML formats plain text: paragraphs split by blank lines, "# "
// starts a heading and "- " list items. Everything is escaped.
func policyHTML(s string) string {
	var b strings.Builder
	for _, para := range strings.Split(strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n"), "\n\n") {
		var text []string
		inList := false
		for _, line := range strings.Split(strings.TrimSpace(para), "\n") {
			line = strings.TrimSpace(line)
			switch {
			case line == "":
			case strings.HasPrefix(line, "# "):
				flushPara(&b, &text)
				if inList {
					b.WriteString("</ul>")
					inList = false
				}
				b.WriteString("<h3>" + html.EscapeString(strings.TrimPrefix(line, "# ")) + "</h3>")
			case strings.HasPrefix(line, "- "):
				flushPara(&b, &text)
				if !inList {
					b.WriteString("<ul>")
					inList = true
				}
				b.WriteString("<li>" + html.EscapeString(strings.TrimPrefix(line, "- ")) + "</li>")
			default:
				if inList {
					b.WriteString("</ul>")
					inList = false
				}
				text = append(text, html.EscapeString(line))
			}
		}
		flushPara(&b, &text)
		if inList {
			b.WriteString("</ul>")
		}
	}
	return b.String()
}

func flushPara(b *strings.Builder, text *[]string) {
	if len(*text) > 0 {
		b.WriteString("<p>" + strings.Join(*text, "<br>") + "</p>")
		*text = nil
	}
}

func (app *App) handlePrivacyPolicy(w http.ResponseWriter, r *http.Request, p *Page) {
	text, updated, def := app.privacyPolicy()
	p.Title, p.Section = "Privacy policy", "about"
	p.PolicyHTML = template.HTML(policyHTML(text))
	p.PolicyUpdated, p.PolicyDefault = updated, def
	app.render(w, "privacy", p)
}

func (app *App) apiPrivacyPolicy(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	text, updated, _ := app.privacyPolicy()
	apiJSON(w, 200, obj{"updated_at": apiTime(updated), "content": policyHTML(text)})
}

func (app *App) handleAdminPrivacyPolicy(w http.ResponseWriter, r *http.Request, p *Page) {
	text := strings.TrimSpace(r.FormValue("privacy_policy"))
	if text == strings.TrimSpace(strings.ReplaceAll(defaultPrivacyPolicy, "{host}", app.cfg.Host)) {
		text = "" // unchanged default stays the default
	}
	app.setSetting("privacy_policy", truncateRunes(text, 20000))
	app.setSetting("privacy_policy_updated", strconv.FormatInt(time.Now().Unix(), 10))
	setFlash(w, "Privacy policy saved.")
	http.Redirect(w, r, "/admin#privacy", http.StatusSeeOther)
}
