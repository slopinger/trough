package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Suggestions while typing a post in the web UI: @accounts (people you
// follow first), #hashtags (most used first) and :custom_emoji:.

type suggestion struct {
	Insert string `json:"insert"` // replaces the typed @, # or : word
	Label  string `json:"label"`
	Sub    string `json:"sub,omitempty"`
	Image  string `json:"image,omitempty"`
}

func likePrefix(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%"
}

func (app *App) autocomplete(me *Account, q string) []suggestion {
	out := []suggestion{}
	if len(q) < 2 {
		return out
	}
	term := q[1:]
	switch q[0] {
	case '@':
		user, domain, _ := strings.Cut(term, "@")
		args := []any{me.ID, likePrefix(strings.ToLower(user)), likePrefix(strings.ToLower(user))}
		where := `(LOWER(a.username) LIKE ? ESCAPE '\' OR LOWER(a.display_name) LIKE ? ESCAPE '\')`
		if domain != "" {
			where += ` AND a.domain LIKE ? ESCAPE '\'`
			args = append(args, likePrefix(strings.ToLower(domain)))
		}
		accs, _ := app.accountsQuery(`SELECT `+accountCols+` FROM accounts a
			LEFT JOIN follows f ON f.account_id=? AND f.target_id=a.id AND f.accepted=1
			WHERE a.suspended=0 AND a.actor_type != 'Application' AND `+where+`
			ORDER BY f.target_id IS NULL, a.domain != '', LENGTH(a.username) LIMIT 6`, args...)
		for _, a := range accs {
			handle := "@" + a.Acct()
			out = append(out, suggestion{Insert: handle, Label: a.Name(), Sub: handle, Image: app.avatarPath(a)})
		}
	case '#':
		rows, err := app.db.Query(`SELECT tag, COUNT(*) n FROM status_tags WHERE tag LIKE ? ESCAPE '\' GROUP BY tag ORDER BY n DESC LIMIT 6`,
			likePrefix(normalizeTag(term)))
		if err == nil {
			for rows.Next() {
				var t string
				var n int
				rows.Scan(&t, &n)
				out = append(out, suggestion{Insert: "#" + t, Label: "#" + t, Sub: plural(n, "post")})
			}
			rows.Close()
		}
	case ':':
		for _, e := range app.customEmojis() {
			if strings.HasPrefix(strings.ToLower(e.Shortcode), strings.ToLower(term)) && len(out) < 8 {
				out = append(out, suggestion{Insert: ":" + e.Shortcode + ":", Label: ":" + e.Shortcode + ":", Image: strings.TrimPrefix(e.URL, app.cfg.BaseURL())})
			}
		}
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func (app *App) avatarPath(a *Account) string {
	if a.Avatar == "" {
		return "/static/avatar.svg"
	}
	return app.mediaURL(a.Avatar)
}

func (app *App) handleAutocomplete(w http.ResponseWriter, r *http.Request, p *Page) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=30")
	json.NewEncoder(w).Encode(app.autocomplete(p.Me, strings.TrimSpace(r.URL.Query().Get("q"))))
}
