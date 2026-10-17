package server

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Weekly activity like Mastodon's /api/v1/instance/activity: local posts,
// active users ("logins") and sign-ups in each of the last 12 weeks, which
// start on Monday 00:00 UTC. A user counts as active in a week once they
// used the web UI or an app token in it; that is recorded in weekly_logins
// whenever last_seen is bumped (at most hourly per user). The admin page
// draws the same numbers as bar charts.

const activityWeeks = 12

func weekStart(t time.Time) time.Time {
	t = t.UTC()
	sinceMonday := (int(t.Weekday()) + 6) % 7
	return time.Date(t.Year(), t.Month(), t.Day()-sinceMonday, 0, 0, 0, 0, time.UTC)
}

// markActive records that the account was active this week.
func (app *App) markActive(accountID string, now time.Time) {
	app.db.Exec(`INSERT OR IGNORE INTO weekly_logins (week, account_id) VALUES (?, ?)`, weekStart(now).Unix(), accountID)
}

type WeekActivity struct {
	Week                            time.Time
	Statuses, Logins, Registrations int
}

// weeklyActivity returns the last 12 weeks, the current one first.
func (app *App) weeklyActivity(now time.Time) []WeekActivity {
	current := weekStart(now)
	from := current.AddDate(0, 0, -7*(activityWeeks-1)).Unix()
	out := make([]WeekActivity, activityWeeks)
	for i := range out {
		out[i].Week = current.AddDate(0, 0, -7*i)
	}
	// each query groups its rows by the week's index counted from the oldest
	count := func(field func(*WeekActivity) *int, q string) {
		rows, err := app.db.Query(q, from)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var idx, n int
			if rows.Scan(&idx, &n) == nil && idx >= 0 && idx < activityWeeks {
				*field(&out[activityWeeks-1-idx]) = n
			}
		}
	}
	count(func(a *WeekActivity) *int { return &a.Statuses },
		`SELECT (created_at - ?1) / 604800, COUNT(*) FROM statuses WHERE local=1 AND boost_of_id='' AND created_at >= ?1 GROUP BY 1`)
	count(func(a *WeekActivity) *int { return &a.Logins },
		`SELECT (week - ?1) / 604800, COUNT(*) FROM weekly_logins WHERE week >= ?1 GROUP BY 1`)
	count(func(a *WeekActivity) *int { return &a.Registrations },
		`SELECT (created_at - ?1) / 604800, COUNT(*) FROM users WHERE created_at >= ?1 GROUP BY 1`)
	return out
}

// GET /api/v1/instance/activity
func (app *App) apiInstanceActivity(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	for _, a := range app.weeklyActivity(time.Now()) {
		out = append(out, obj{"week": strconv.FormatInt(a.Week.Unix(), 10), "statuses": strconv.Itoa(a.Statuses),
			"logins": strconv.Itoa(a.Logins), "registrations": strconv.Itoa(a.Registrations)})
	}
	apiJSON(w, 200, out)
}

// ActivityChart is one bar chart on the admin page, oldest week left.
type ActivityChart struct {
	Title     string
	ThisWeek  int
	Bars      []ActivityBar
	FirstWeek time.Time
}

type ActivityBar struct {
	X, Y, H int
	Title   string
}

const chartHeight = 60

func activityCharts(weeks []WeekActivity) []ActivityChart {
	series := []struct {
		title, unit string
		n           func(WeekActivity) int
	}{
		{"Local posts", "posts", func(a WeekActivity) int { return a.Statuses }},
		{"Active users", "active users", func(a WeekActivity) int { return a.Logins }},
		{"Sign-ups", "sign-ups", func(a WeekActivity) int { return a.Registrations }},
	}
	var charts []ActivityChart
	for _, s := range series {
		most := 1
		for _, w := range weeks {
			most = max(most, s.n(w))
		}
		ch := ActivityChart{Title: s.title, ThisWeek: s.n(weeks[0]), FirstWeek: weeks[len(weeks)-1].Week}
		for i := len(weeks) - 1; i >= 0; i-- {
			n := s.n(weeks[i])
			h := max(1, n*chartHeight/most) // a sliver marks empty weeks
			ch.Bars = append(ch.Bars, ActivityBar{X: (len(weeks) - 1 - i) * 20, Y: chartHeight - h, H: h,
				Title: fmt.Sprintf("Week of %s: %d %s", weeks[i].Week.Format("2 Jan"), n, s.unit)})
		}
		charts = append(charts, ch)
	}
	return charts
}
