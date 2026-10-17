package server

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Annual reports (Mastodon 4.3's "Wrapstodon"): a look back at one
// account's year here. The web page is /annual_report[/{year}]; clients
// get Mastodon's entity from /api/v1/annual_reports. Reports are computed
// when asked for, from the account's own posts, follows and the
// interactions stored here; the year in progress is shown "so far".

type AnnualReport struct {
	Year                                 int
	SoFar                                bool
	Total, Standalone, Replies, Boosts   int
	Polls                                int
	Archetype                            string
	Months                               [12]MonthStat
	Chart                                ActivityChart
	TopTags                              []TagCount
	TopByBoosts, TopByFavs, TopByReplies *TopStatus
	Interacted                           []AccountCount
	NewFollowers, NewFollowing           int
	PostsPercentile, FollowersPercentile int // share of local accounts with fewer, in %
}

type MonthStat struct{ Posts, Following, Followers int }

type TagCount struct {
	Name  string
	Count int
}

type TopStatus struct {
	Status *Status
	Count  int
}

type AccountCount struct {
	Account *Account
	Count   int
}

func yearBounds(year int) (int64, int64) {
	return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
}

// reportYears are the years in which the account posted, newest first.
func (app *App) reportYears(me *Account) []int {
	rows, err := app.db.Query(`SELECT DISTINCT CAST(strftime('%Y', created_at, 'unixepoch') AS INTEGER) y FROM statuses
		WHERE account_id=? ORDER BY y DESC`, me.ID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var y int
		if rows.Scan(&y) == nil {
			out = append(out, y)
		}
	}
	return out
}

func (app *App) annualReport(me *Account, year int) *AnnualReport {
	from, to := yearBounds(year)
	r := &AnnualReport{Year: year, SoFar: time.Now().Unix() < to}
	app.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(boost_of_id != ''),0),
		COALESCE(SUM(boost_of_id = '' AND in_reply_to_uri != '' AND in_reply_to_account_id != account_id),0),
		COALESCE(SUM(boost_of_id = '' AND poll != ''),0)
		FROM statuses WHERE account_id=? AND created_at >= ? AND created_at < ?`, me.ID, from, to).Scan(&r.Total, &r.Boosts, &r.Replies, &r.Polls)
	r.Standalone = r.Total - r.Boosts - r.Replies
	r.Archetype = archetype(r)

	monthly := func(q string, add func(*MonthStat, int)) {
		rows, err := app.db.Query(q, me.ID, from, to)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var m, n int
			if rows.Scan(&m, &n) == nil && m >= 1 && m <= 12 {
				add(&r.Months[m-1], n)
			}
		}
	}
	month := `CAST(strftime('%m', created_at, 'unixepoch') AS INTEGER)`
	monthly(`SELECT `+month+`, COUNT(*) FROM statuses WHERE account_id=? AND created_at >= ? AND created_at < ? GROUP BY 1`,
		func(s *MonthStat, n int) { s.Posts = n })
	monthly(`SELECT `+month+`, COUNT(*) FROM follows WHERE account_id=? AND accepted=1 AND created_at >= ? AND created_at < ? GROUP BY 1`,
		func(s *MonthStat, n int) { s.Following = n })
	monthly(`SELECT `+month+`, COUNT(*) FROM follows WHERE target_id=? AND accepted=1 AND created_at >= ? AND created_at < ? GROUP BY 1`,
		func(s *MonthStat, n int) { s.Followers = n })
	for _, m := range r.Months {
		r.NewFollowers += m.Followers
		r.NewFollowing += m.Following
	}
	r.Chart = monthChart(r)

	if rows, err := app.db.Query(`SELECT t.tag, COUNT(*) n FROM status_tags t JOIN statuses s ON s.id=t.status_id
		WHERE s.account_id=? AND s.boost_of_id='' AND s.created_at >= ? AND s.created_at < ? GROUP BY t.tag ORDER BY n DESC, t.tag LIMIT 5`, me.ID, from, to); err == nil {
		for rows.Next() {
			var tc TagCount
			if rows.Scan(&tc.Name, &tc.Count) == nil {
				r.TopTags = append(r.TopTags, tc)
			}
		}
		rows.Close()
	}

	top := func(count string) *TopStatus {
		var id string
		var n int
		if app.db.QueryRow(`SELECT s.id, (`+count+`) n FROM statuses s WHERE s.account_id=? AND s.boost_of_id='' AND s.visibility IN ('public','unlisted')
			AND s.created_at >= ? AND s.created_at < ? ORDER BY n DESC, s.id DESC LIMIT 1`, me.ID, from, to).Scan(&id, &n) != nil || n == 0 {
			return nil
		}
		if sts, err := app.queryStatuses(me, `s.id=?`, []any{id}, "", 0, false); err == nil && len(sts) == 1 {
			return &TopStatus{sts[0], n}
		}
		return nil
	}
	r.TopByBoosts = top(`SELECT COUNT(*) FROM statuses b WHERE b.boost_of_id=s.id`)
	r.TopByFavs = top(`SELECT COUNT(*) FROM likes l WHERE l.status_id=s.id`)
	r.TopByReplies = top(`SELECT COUNT(*) FROM statuses c WHERE c.in_reply_to_id=s.id AND c.account_id != s.account_id`)

	// who they replied to, boosted and favourited most
	if rows, err := app.db.Query(`SELECT id, COUNT(*) n FROM (
			SELECT in_reply_to_account_id id FROM statuses WHERE account_id=? AND boost_of_id='' AND in_reply_to_account_id NOT IN ('', ?) AND created_at >= ? AND created_at < ?
			UNION ALL SELECT o.account_id FROM statuses b JOIN statuses o ON o.id=b.boost_of_id WHERE b.account_id=? AND o.account_id != ? AND b.created_at >= ? AND b.created_at < ?
			UNION ALL SELECT o.account_id FROM likes l JOIN statuses o ON o.id=l.status_id WHERE l.account_id=? AND o.account_id != ? AND l.created_at >= ? AND l.created_at < ?
		) GROUP BY id ORDER BY n DESC, id LIMIT 5`, me.ID, me.ID, from, to, me.ID, me.ID, from, to, me.ID, me.ID, from, to); err == nil {
		var ids []string
		counts := map[string]int{}
		for rows.Next() {
			var id string
			var n int
			if rows.Scan(&id, &n) == nil {
				ids = append(ids, id)
				counts[id] = n
			}
		}
		rows.Close()
		accs, _ := app.accountsByIDs(ids)
		for _, id := range ids {
			if a := accs[id]; a != nil && !a.Suspended {
				r.Interacted = append(r.Interacted, AccountCount{a, counts[id]})
			}
		}
	}

	r.PostsPercentile = app.percentile(`SELECT COUNT(*) FROM statuses s WHERE s.account_id=u.account_id AND s.created_at >= ? AND s.created_at < ?`, me.ID, from, to)
	r.FollowersPercentile = app.percentile(`SELECT COUNT(*) FROM follows f WHERE f.target_id=u.account_id AND f.accepted=1 AND f.created_at >= ? AND f.created_at < ?`, me.ID, from, to)
	return r
}

// percentile is the share of local accounts whose count (a subquery on
// users u) is lower than me's.
func (app *App) percentile(count, me string, from, to int64) int {
	var lower, total int
	app.db.QueryRow(`WITH c AS (SELECT u.account_id id, (`+count+`) n FROM users u WHERE u.approved=1)
		SELECT (SELECT COUNT(*) FROM c WHERE n < (SELECT n FROM c WHERE id=?)), (SELECT COUNT(*) FROM c)`, from, to, me).Scan(&lower, &total)
	if total == 0 {
		return 0
	}
	return lower * 100 / total
}

// archetype follows Mastodon's rules for the year's kind of poster.
func archetype(r *AnnualReport) string {
	switch {
	case r.Total < 10:
		return "lurker"
	case r.Boosts > r.Standalone*2:
		return "booster"
	case r.Polls*10 > r.Standalone:
		return "pollster"
	case r.Replies > r.Standalone*2:
		return "replier"
	}
	return "oracle"
}

var archetypes = map[string][2]string{
	"lurker":   {"The lurker", "You mostly read along this year. Quiet company counts too."},
	"booster":  {"The booster", "You shared other people's posts far more than your own and got them in front of new readers."},
	"pollster": {"The pollster", "You kept asking people what they think, with polls in plenty of your posts."},
	"replier":  {"The replier", "You were in the replies, turning other people's posts into conversations."},
	"oracle":   {"The oracle", "You started things: most of what you wrote were new posts rather than replies."},
}

func (r *AnnualReport) ArchetypeTitle() string { return archetypes[r.Archetype][0] }
func (r *AnnualReport) ArchetypeText() string  { return archetypes[r.Archetype][1] }

func (r *AnnualReport) ArchetypeIcon() string {
	return map[string]string{"lurker": "search", "booster": "boost", "pollster": "poll", "replier": "reply", "oracle": "pen"}[r.Archetype]
}

func monthChart(r *AnnualReport) ActivityChart {
	most := 1
	for _, m := range r.Months {
		most = max(most, m.Posts)
	}
	ch := ActivityChart{Title: "Posts per month", ThisWeek: r.Total}
	for i, m := range r.Months {
		h := max(1, m.Posts*chartHeight/most)
		ch.Bars = append(ch.Bars, ActivityBar{X: i * 20, Y: chartHeight - h, H: h,
			Title: fmt.Sprintf("%s %d: %s", time.Month(i+1), r.Year, plural(m.Posts, "post"))})
	}
	return ch
}

func (app *App) apiAnnualReport(r *AnnualReport) obj {
	series := []any{}
	for i, m := range r.Months {
		series = append(series, obj{"month": i + 1, "statuses": m.Posts, "following": m.Following, "followers": m.Followers})
	}
	tags := []any{}
	for _, t := range r.TopTags {
		tags = append(tags, obj{"name": t.Name, "count": t.Count})
	}
	accs := []any{}
	for _, a := range r.Interacted {
		accs = append(accs, obj{"account_id": a.Account.ID, "count": a.Count})
	}
	topID := func(t *TopStatus) any {
		if t == nil {
			return nil
		}
		return t.Status.ID
	}
	return obj{"year": r.Year, "schema_version": 2, "data": obj{
		"archetype":   r.Archetype,
		"time_series": series, "top_hashtags": tags, "most_used_apps": []any{},
		"top_statuses":                      obj{"by_reblogs": topID(r.TopByBoosts), "by_favourites": topID(r.TopByFavs), "by_replies": topID(r.TopByReplies)},
		"commonly_interacted_with_accounts": accs,
		"type_distribution":                 obj{"total": r.Total, "reblogs": r.Boosts, "replies": r.Replies, "standalone": r.Standalone},
		"percentiles":                       obj{"followers": r.FollowersPercentile, "statuses": r.PostsPercentile},
	}}
}

// apiAnnualReports answers like Mastodon: the reports with the accounts and statuses they refer to.
func (app *App) apiAnnualReports(me *Account, reports []*AnnualReport) obj {
	out, accounts, statuses := []any{}, []any{}, []any{}
	seenSt := map[string]bool{}
	for _, r := range reports {
		out = append(out, app.apiAnnualReport(r))
		for _, a := range r.Interacted {
			accounts = append(accounts, app.apiAccount(a.Account))
		}
		for _, t := range []*TopStatus{r.TopByBoosts, r.TopByFavs, r.TopByReplies} {
			if t != nil && !seenSt[t.Status.ID] {
				seenSt[t.Status.ID] = true
				statuses = append(statuses, app.apiStatus(me, t.Status))
			}
		}
	}
	return obj{"annual_reports": out, "accounts": accounts, "statuses": statuses}
}

// GET /api/v1/annual_reports
func (app *App) apiAnnualReportList(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var reports []*AnnualReport
	for _, y := range app.reportYears(c.Me) {
		reports = append(reports, app.annualReport(c.Me, y))
	}
	apiJSON(w, 200, app.apiAnnualReports(c.Me, reports))
}

// GET /api/v1/annual_reports/{year}, /api/v1/annual_reports/{year}/state, POST .../read and .../generate
func (app *App) apiAnnualReportYear(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	year, err := strconv.Atoi(r.PathValue("year"))
	has := false
	for _, y := range app.reportYears(c.Me) {
		has = has || y == year
	}
	switch {
	case r.PathValue("what") == "state":
		state := "ineligible"
		if has {
			state = "available"
		}
		apiJSON(w, 200, obj{"state": state})
	case err != nil || !has:
		apiError(w, 404, "Record not found")
	case r.Method == http.MethodPost:
		apiJSON(w, 200, obj{})
	default:
		apiJSON(w, 200, app.apiAnnualReports(c.Me, []*AnnualReport{app.annualReport(c.Me, year)}))
	}
}

// reportBannerYear is the year Home invites the account to look back on:
// from December to mid-January, like Mastodon, if it posted that year.
func (app *App) reportBannerYear(me *Account, now time.Time) int {
	year := now.Year()
	switch {
	case now.Month() == time.January && now.Day() <= 15:
		year--
	case now.Month() != time.December:
		return 0
	}
	from, to := yearBounds(year)
	if app.countInt(`SELECT COUNT(*) FROM statuses WHERE account_id=? AND created_at >= ? AND created_at < ?`, me.ID, from, to) == 0 {
		return 0
	}
	return year
}

// GET /annual_report[/{year}]
func (app *App) handleAnnualReport(w http.ResponseWriter, r *http.Request, p *Page) {
	years := app.reportYears(p.Me)
	year := time.Now().UTC().Year()
	if y, err := strconv.Atoi(r.PathValue("year")); err == nil {
		year = y
	} else if len(years) > 0 {
		year = years[0]
	}
	p.Title, p.Section = fmt.Sprintf("Your %d", year), "profile"
	p.AnnualReport = app.annualReport(p.Me, year)
	p.Years = years
	app.render(w, "annualreport", p)
}

// TopPost is a post that leads one or more of the year's lists.
type TopPost struct {
	Label  string
	Status *Status
}

// TopPosts merges the year's most boosted, favourited and replied-to posts,
// so a post that leads several lists is shown once.
func (r *AnnualReport) TopPosts() []TopPost {
	var out []TopPost
	for _, t := range []struct {
		label string
		top   *TopStatus
		units [2]string
	}{{"Most boosted", r.TopByBoosts, [2]string{"boost", "boosts"}}, {"Most favourited", r.TopByFavs, [2]string{"favourite", "favourites"}},
		{"Most replied to", r.TopByReplies, [2]string{"reply", "replies"}}} {
		if t.top == nil {
			continue
		}
		unit := t.units[1]
		if t.top.Count == 1 {
			unit = t.units[0]
		}
		label := t.label + " (" + strconv.Itoa(t.top.Count) + " " + unit + ")"
		merged := false
		for i := range out {
			if out[i].Status.ID == t.top.Status.ID {
				out[i].Label += " · " + label
				merged = true
			}
		}
		if !merged {
			out = append(out, TopPost{label, t.top.Status})
		}
	}
	return out
}
