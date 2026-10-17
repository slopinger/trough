package server

import (
	"encoding/json"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Terms of service (Mastodon 4.4): the admin publishes versions with the
// date they take effect. /terms-of-service shows the one in effect and
// announces an upcoming one, /terms-of-service/:date shows any version, and
// GET /api/v1/instance/terms_of_service[/:date] gives them to apps. There is
// no default text: without a published version both answer 404, like
// Mastodon. Versions are kept as JSON in the terms_of_service setting.

type TermsVersion struct {
	Date      string `json:"date"` // effective date, YYYY-MM-DD (UTC)
	Text      string `json:"text"`
	Published int64  `json:"published"`
}

// termsPage is what the terms page and the admin list show.
type termsPage struct {
	TermsVersion
	Effective bool   // in effect today
	Next      string // date of the version that replaces it, if any
	Versions  []termsRow
}

type termsRow struct {
	Date, Status string // Status: "in effect", "upcoming" or "previous"
}

const dateOnly = "2006-01-02"

// longDate shows 2026-10-20 as "20 October 2026".
func longDate(d string) string {
	t, err := time.Parse(dateOnly, d)
	if err != nil {
		return d
	}
	return t.Format("2 January 2006")
}

func (v TermsVersion) Long() string  { return longDate(v.Date) }
func (r termsRow) Long() string      { return longDate(r.Date) }
func (t termsPage) NextLong() string { return longDate(t.Next) }

func todayUTC() string { return time.Now().UTC().Format(dateOnly) }

// termsVersions returns the published versions, oldest first.
func (app *App) termsVersions() []TermsVersion {
	var vs []TermsVersion
	json.Unmarshal([]byte(app.getSetting("terms_of_service")), &vs)
	sort.Slice(vs, func(i, j int) bool { return vs[i].Date < vs[j].Date })
	return vs
}

func (app *App) saveTermsVersions(vs []TermsVersion) {
	if len(vs) == 0 {
		app.setSetting("terms_of_service", "")
		return
	}
	b, _ := json.Marshal(vs)
	app.setSetting("terms_of_service", string(b))
}

// terms finds the version for date ("" means the current one: the newest in
// effect, or the first upcoming one if none is yet).
func (app *App) terms(date string) (*termsPage, bool) {
	vs := app.termsVersions()
	today := todayUTC()
	i := -1
	for j, v := range vs {
		if (date == "" && v.Date <= today) || (date != "" && v.Date == date) {
			i = j
		}
	}
	if i < 0 && date == "" && len(vs) > 0 {
		i = 0
	}
	if i < 0 {
		return nil, false
	}
	t := &termsPage{TermsVersion: vs[i], Effective: vs[i].Date <= today}
	if i+1 < len(vs) {
		t.Next = vs[i+1].Date
	}
	t.Versions = termsRows(vs, today)
	return t, true
}

// termsRows lists every version newest first with its state.
func termsRows(vs []TermsVersion, today string) []termsRow {
	current := ""
	for _, v := range vs {
		if v.Date <= today {
			current = v.Date
		}
	}
	var rows []termsRow
	for i := len(vs) - 1; i >= 0; i-- {
		st := "previous"
		switch {
		case vs[i].Date > today:
			st = "upcoming"
		case vs[i].Date == current:
			st = "in effect"
		}
		rows = append(rows, termsRow{vs[i].Date, st})
	}
	return rows
}

// termsURL is the instance entity's urls.terms_of_service, null without terms.
func (app *App) termsURL() any {
	if app.getSetting("terms_of_service") == "" {
		return nil
	}
	return app.cfg.BaseURL() + "/terms-of-service"
}

func (app *App) handleTerms(w http.ResponseWriter, r *http.Request, p *Page) {
	t, ok := app.terms(r.PathValue("date"))
	if !ok {
		app.notFound(w, p)
		return
	}
	p.Title, p.Section = "Terms of service", "about"
	p.Terms = t
	p.PolicyHTML = template.HTML(policyHTML(t.Text))
	app.render(w, "terms", p)
}

func (app *App) apiTerms(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	t, ok := app.terms(r.PathValue("date"))
	if !ok {
		apiError(w, 404, "Record not found")
		return
	}
	var next any
	if t.Next != "" {
		next = t.Next
	}
	apiJSON(w, 200, obj{"effective_date": t.Date, "effective": t.Effective, "content": policyHTML(t.Text), "succeeded_by": next})
}

// handleAdminTerms publishes a version (replacing one with the same date)
// or removes an upcoming one. Versions already in effect stay as they are:
// people agreed to them.
func (app *App) handleAdminTerms(w http.ResponseWriter, r *http.Request, p *Page) {
	vs, today := app.termsVersions(), todayUTC()
	date := r.FormValue("date")
	keep := vs[:0]
	if r.FormValue("delete") != "" {
		for _, v := range vs {
			if v.Date != date || v.Date <= today {
				keep = append(keep, v)
			}
		}
		app.saveTermsVersions(keep)
		setFlash(w, "Upcoming terms removed.")
		http.Redirect(w, r, "/admin#terms", http.StatusSeeOther)
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))
	if _, err := time.Parse(dateOnly, date); err != nil || date < today || text == "" {
		setFlash(w, "The terms need a text and a date from today on.")
		http.Redirect(w, r, "/admin#terms", http.StatusSeeOther)
		return
	}
	for _, v := range vs {
		if v.Date != date {
			keep = append(keep, v)
		}
	}
	keep = append(keep, TermsVersion{Date: date, Text: truncateRunes(text, 20000), Published: time.Now().Unix()})
	app.saveTermsVersions(keep)
	setFlash(w, "Terms of service published.")
	http.Redirect(w, r, "/admin#terms", http.StatusSeeOther)
}

// adminTerms fills the admin form: every version, the newest text to start
// from, and a date 30 days ahead when terms are already in effect (people
// should hear about changes in advance).
func (app *App) adminTerms(p *Page) {
	vs, today := app.termsVersions(), todayUTC()
	p.TermsRows = termsRows(vs, today)
	p.TermsDate = today
	if len(vs) > 0 {
		p.TermsSource = vs[len(vs)-1].Text
		if vs[0].Date <= today {
			p.TermsDate = time.Now().UTC().AddDate(0, 0, 30).Format(dateOnly)
		}
	}
}
