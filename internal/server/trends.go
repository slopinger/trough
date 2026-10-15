package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Trends: what is popular here lately, for apps' Explore tabs and the web
// UI's Explore page. Computed on request from recent public posts this
// instance knows about. A hashtag or link only trends once several accounts
// use it, so one account can't push its own.

const (
	trendDays        = 7
	trendMinAccounts = 2
	trendPostWindow  = 3 * 24 * time.Hour
)

// TagDay is one day of a hashtag's or link's use (Mastodon's history entries).
type TagDay struct {
	Day            int64 // unix time of the day's start (UTC)
	Uses, Accounts int
}

// TrendTag is a trending hashtag with its last trendDays days, newest first.
type TrendTag struct {
	Name     string
	Accounts int
	History  []TagDay
}

// TrendLink is a trending link card.
type TrendLink struct {
	Card     *LinkCard
	Accounts int
	History  []TagDay
}

func trendSince() int64 {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(trendDays - 1)).Unix()
}

// dayHistory turns (day, uses, accounts) rows into trendDays entries, newest first.
func dayHistory(rows map[int64][2]int) []TagDay {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	out := make([]TagDay, trendDays)
	for i := range out {
		d := today.AddDate(0, 0, -i).Unix()
		out[i] = TagDay{Day: d, Uses: rows[d][0], Accounts: rows[d][1]}
	}
	return out
}

const trendBase = ` FROM statuses s JOIN accounts a ON a.id=s.account_id
	WHERE s.visibility='public' AND s.boost_of_id='' AND a.suspended=0 AND s.created_at >= ?`

// trendingTags returns hashtags used by at least trendMinAccounts accounts
// in the last trendDays days, most accounts first.
func (app *App) trendingTags(limit, offset int) []TrendTag {
	since := trendSince()
	rows, err := app.db.Query(`SELECT t.tag, COUNT(DISTINCT s.account_id) n FROM status_tags t JOIN statuses s ON s.id=t.status_id
		JOIN accounts a ON a.id=s.account_id WHERE s.visibility='public' AND s.boost_of_id='' AND a.suspended=0 AND s.created_at >= ?
		GROUP BY t.tag HAVING n >= ? ORDER BY n DESC, MAX(s.created_at) DESC LIMIT ? OFFSET ?`, since, trendMinAccounts, limit, offset)
	if err != nil {
		return nil
	}
	var out []TrendTag
	for rows.Next() {
		var t TrendTag
		rows.Scan(&t.Name, &t.Accounts)
		out = append(out, t)
	}
	rows.Close()
	for i := range out {
		out[i].History = app.dayCounts(`SELECT s.created_at/86400*86400 d, COUNT(*), COUNT(DISTINCT s.account_id) FROM status_tags t
			JOIN statuses s ON s.id=t.status_id JOIN accounts a ON a.id=s.account_id
			WHERE t.tag=? AND s.visibility='public' AND s.boost_of_id='' AND a.suspended=0 AND s.created_at >= ? GROUP BY d`, out[i].Name, since)
	}
	return out
}

// tagHistory is the last trendDays days of a hashtag, for Tag entities.
func (app *App) tagHistory(tag string) []TagDay {
	return app.dayCounts(`SELECT s.created_at/86400*86400 d, COUNT(*), COUNT(DISTINCT s.account_id) FROM status_tags t
		JOIN statuses s ON s.id=t.status_id JOIN accounts a ON a.id=s.account_id
		WHERE t.tag=? AND s.visibility='public' AND s.boost_of_id='' AND a.suspended=0 AND s.created_at >= ? GROUP BY d`, tag, trendSince())
}

func (app *App) dayCounts(q string, args ...any) []TagDay {
	counts := map[int64][2]int{}
	if rows, err := app.db.Query(q, args...); err == nil {
		for rows.Next() {
			var d int64
			var uses, accs int
			rows.Scan(&d, &uses, &accs)
			counts[d] = [2]int{uses, accs}
		}
		rows.Close()
	}
	return dayHistory(counts)
}

// trendingLinks returns link cards shared by at least trendMinAccounts accounts.
func (app *App) trendingLinks(limit, offset int) []TrendLink {
	since := trendSince()
	rows, err := app.db.Query(`SELECT s.card_url, COUNT(DISTINCT s.account_id) n`+trendBase+` AND s.card_url != ''
		GROUP BY s.card_url HAVING n >= ? ORDER BY n DESC, MAX(s.created_at) DESC LIMIT ? OFFSET ?`, since, trendMinAccounts, limit, offset)
	if err != nil {
		return nil
	}
	var out []TrendLink
	var urls []string
	for rows.Next() {
		var l TrendLink
		var u string
		rows.Scan(&u, &l.Accounts)
		l.Card = &LinkCard{URL: u}
		urls = append(urls, u)
		out = append(out, l)
	}
	rows.Close()
	for i := range out {
		c := out[i].Card
		app.db.QueryRow(`SELECT title, description, image, site FROM link_cards WHERE url=?`, c.URL).Scan(&c.Title, &c.Description, &c.Image, &c.Site)
		out[i].History = app.dayCounts(`SELECT s.created_at/86400*86400 d, COUNT(*), COUNT(DISTINCT s.account_id)`+trendBase+
			` AND s.card_url=? GROUP BY d`, since, c.URL)
	}
	return out
}

// trendingStatuses returns recent public posts with the most interaction
// (likes, boosts and other people's replies), decaying with age.
func (app *App) trendingStatuses(v *Account, limit, offset int) []*Status {
	now := time.Now().Unix()
	rows, err := app.db.Query(`SELECT id FROM (SELECT s.id, s.created_at,
			(SELECT COUNT(*) FROM likes l WHERE l.status_id=s.id)
			+ 2*(SELECT COUNT(*) FROM statuses b WHERE b.boost_of_id=s.id)
			+ (SELECT COUNT(*) FROM statuses r WHERE r.in_reply_to_id=s.id AND r.account_id != s.account_id) score`+
		trendBase+`) WHERE score >= 2 ORDER BY score / ((? - created_at) / 3600.0 + 2) DESC LIMIT ? OFFSET ?`,
		now-int64(trendPostWindow.Seconds()), now, limit, offset)
	if err != nil {
		return nil
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil
	}
	mute, margs := muteSQL(v)
	q, args := inClause(`s.id IN (%s)`, ids)
	sts, _ := app.queryStatuses(v, q+` AND `+mute, append(args, margs...), "", 0, false)
	rank := map[string]int{}
	for i, id := range ids {
		rank[id] = i
	}
	out := make([]*Status, len(ids))
	for _, s := range sts {
		out[rank[s.ID]] = s
	}
	res := out[:0]
	for _, s := range out {
		if s != nil {
			res = append(res, s)
		}
	}
	return res
}

func apiHistory(h []TagDay) []any {
	out := []any{}
	for _, d := range h {
		out = append(out, obj{"day": strconv.FormatInt(d.Day, 10), "uses": strconv.Itoa(d.Uses), "accounts": strconv.Itoa(d.Accounts)})
	}
	return out
}

func trendLimits(r *http.Request) (int, int) {
	limit, offset := 10, 0
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 20)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = n
	}
	return limit, offset
}

func (app *App) apiTrendTags(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	limit, offset := trendLimits(r)
	out := []any{}
	for _, t := range app.trendingTags(limit, offset) {
		m := app.apiTag(t.Name, c.Me != nil && app.followsTag(c.Me, t.Name))
		m["history"] = apiHistory(t.History)
		out = append(out, m)
	}
	apiJSON(w, 200, out)
}

func (app *App) apiTrendStatuses(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	limit, offset := trendLimits(r)
	out := []any{}
	for _, s := range app.trendingStatuses(c.Me, limit, offset) {
		out = append(out, app.apiStatus(c.Me, s))
	}
	apiJSON(w, 200, out)
}

func (app *App) apiTrendLinks(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	limit, offset := trendLimits(r)
	out := []any{}
	for _, l := range app.trendingLinks(limit, offset) {
		m := app.apiCard(l.Card).(obj)
		m["history"] = apiHistory(l.History)
		out = append(out, m)
	}
	apiJSON(w, 200, out)
}

// handleExplore is the web UI's Explore page: trending posts, hashtags and links.
func (app *App) handleExplore(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section, p.Heading = "Explore", "explore", "Explore"
	p.Tab = r.URL.Query().Get("tab")
	switch p.Tab {
	case "tags":
		p.TrendTags = app.trendingTags(20, 0)
	case "links":
		p.TrendLinks = app.trendingLinks(20, 0)
	case "people":
		if p.Me == nil {
			http.Redirect(w, r, "/login?next=/explore%3Ftab%3Dpeople", http.StatusFound)
			return
		}
		p.Suggestions = app.suggestions(p.Me, 20)
	default:
		p.Tab = "posts"
		p.Statuses = app.applyFilters(p.Me, app.trendingStatuses(p.Me, 20, 0), "public")
		p.Empty = "Nothing is trending right now. Posts that get likes, boosts and replies show up here."
	}
	app.render(w, "explore", p)
}

// tagUses sums a history's uses, for the Explore page.
func tagUses(h []TagDay) int {
	n := 0
	for _, d := range h {
		n += d.Uses
	}
	return n
}

// sparkline draws a history (newest first) as SVG polyline points, oldest left.
func sparkline(h []TagDay) string {
	peak := 1
	for _, d := range h {
		peak = max(peak, d.Uses)
	}
	var b strings.Builder
	for i := range h {
		d := h[len(h)-1-i]
		x := i * 60 / max(len(h)-1, 1)
		y := 22 - d.Uses*20/peak
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.Itoa(x) + "," + strconv.Itoa(y))
	}
	return b.String()
}
