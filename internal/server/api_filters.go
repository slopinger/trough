package server

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Mastodon filters API: v2 filter groups with keywords, and the older v1
// filters, where each keyword shows as a filter of its own.

func (app *App) apiFilterV2(g *FilterGroup) obj {
	kws := []any{}
	for _, k := range g.Keywords {
		kws = append(kws, apiFilterKeyword(k))
	}
	return obj{"id": g.ID, "title": g.Title, "context": g.Contexts(), "expires_at": filterExpiry(g),
		"filter_action": g.Action, "keywords": kws, "statuses": []any{}}
}

func apiFilterKeyword(k *Filter) obj {
	return obj{"id": k.ID, "keyword": k.Phrase, "whole_word": k.WholeWord}
}

func apiFilterV1(k *Filter) obj {
	return obj{"id": k.ID, "phrase": k.Phrase, "context": k.Group.Contexts(), "whole_word": k.WholeWord,
		"expires_at": filterExpiry(k.Group), "irreversible": k.Action == "hide"}
}

func filterExpiry(g *FilterGroup) any {
	if g.ExpiresAt == 0 {
		return nil
	}
	return apiTime(time.Unix(g.ExpiresAt, 0))
}

// apiFilterResult is a status's "filtered" entry.
func (app *App) apiFilterResult(s *Status) []any {
	if s.Filter == nil {
		return []any{}
	}
	f := app.apiFilterV2(s.Filter.Group)
	return []any{obj{"filter": f, "keyword_matches": []string{s.Filter.Phrase}, "status_matches": nil}}
}

// readFilterGroup applies title, context, filter_action and expires_in
// from a request; absent fields stay as they are.
func readFilterGroup(p *apiParams, g *FilterGroup) {
	if p.Has("title") {
		g.Title = p.Get("title")
	}
	if p.Has("context") {
		g.Context = p.List("context")
	}
	if p.Has("filter_action") {
		g.Action = p.Get("filter_action") // warn, hide or blur (4.4)
	}
	readExpiry(p, g)
}

func readExpiry(p *apiParams, g *FilterGroup) {
	if !p.Has("expires_in") {
		return
	}
	g.ExpiresAt = 0 // an empty value removes the expiry
	if secs, err := strconv.ParseInt(strings.TrimSpace(p.Get("expires_in")), 10, 64); err == nil && secs > 0 {
		g.ExpiresAt = time.Now().Unix() + secs
	}
}

type keywordAttr struct {
	ID, Keyword string
	WholeWord   *bool
	Destroy     bool
}

// keywordAttrs reads keywords_attributes, as JSON objects or Rails-style
// form fields (keywords_attributes[][keyword] or keywords_attributes[0][keyword]).
func keywordAttrs(p *apiParams) []keywordAttr {
	attrBool := func(v any) *bool {
		var b bool
		switch x := v.(type) {
		case bool:
			b = x
		case string:
			b = x == "true" || x == "1"
		case float64:
			b = x != 0
		default:
			return nil
		}
		return &b
	}
	attrStr := func(v any) string {
		switch x := v.(type) {
		case string:
			return x
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64)
		}
		return ""
	}
	fromMap := func(m map[string]any) keywordAttr {
		a := keywordAttr{ID: attrStr(m["id"]), Keyword: attrStr(m["keyword"]), WholeWord: attrBool(m["whole_word"])}
		if d := attrBool(m["_destroy"]); d != nil {
			a.Destroy = *d
		}
		return a
	}
	var out []keywordAttr
	if v, ok := p.jsonPath("keywords_attributes"); ok {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				if m, ok := e.(map[string]any); ok {
					out = append(out, fromMap(m))
				}
			}
		case map[string]any: // {"0": {...}, "1": {...}}
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { a, _ := strconv.Atoi(keys[i]); b, _ := strconv.Atoi(keys[j]); return a < b })
			for _, k := range keys {
				if m, ok := x[k].(map[string]any); ok {
					out = append(out, fromMap(m))
				}
			}
		}
		return out
	}
	form := p.r.Form
	if p.r.MultipartForm != nil {
		form = p.r.MultipartForm.Value
	}
	field := func(prefix, name string) []string { return form[prefix+"["+name+"]"] }
	collect := func(prefix string) {
		kws, ids, whole, destroy := field(prefix, "keyword"), field(prefix, "id"), field(prefix, "whole_word"), field(prefix, "_destroy")
		n := max(len(kws), len(ids))
		for i := 0; i < n; i++ {
			a := keywordAttr{}
			if i < len(kws) {
				a.Keyword = kws[i]
			}
			if i < len(ids) {
				a.ID = ids[i]
			}
			if i < len(whole) {
				a.WholeWord = attrBool(whole[i])
			}
			if i < len(destroy) {
				a.Destroy = *attrBool(destroy[i])
			}
			out = append(out, a)
		}
	}
	collect("keywords_attributes[]")
	for i := 0; i < maxFilterKeywords; i++ {
		prefix := "keywords_attributes[" + strconv.Itoa(i) + "]"
		if len(field(prefix, "keyword")) == 0 && len(field(prefix, "id")) == 0 {
			break
		}
		collect(prefix)
	}
	return out
}

// applyKeywordAttrs adds, changes and removes a group's keywords.
func (app *App) applyKeywordAttrs(me *Account, g *FilterGroup, attrs []keywordAttr) error {
	byID := map[string]*Filter{}
	for _, k := range g.Keywords {
		byID[k.ID] = k
	}
	for _, a := range attrs {
		whole := true
		if a.WholeWord != nil {
			whole = *a.WholeWord
		}
		switch k := byID[a.ID]; {
		case a.ID != "" && k == nil:
			continue // not one of this group's keywords
		case k != nil && a.Destroy:
			app.DeleteFilterKeyword(me, k.ID)
		case k != nil:
			if a.WholeWord == nil {
				whole = k.WholeWord
			}
			phrase := a.Keyword
			if phrase == "" {
				phrase = k.Phrase
			}
			if err := app.UpdateFilterKeyword(me, k.ID, phrase, whole); err != nil {
				return err
			}
		case !a.Destroy:
			if _, err := app.AddFilterKeyword(me, g.ID, a.Keyword, whole); err != nil {
				return err
			}
		}
	}
	return nil
}

func (app *App) apiFiltersV2(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	for _, g := range app.filterGroups(c.Me.ID) {
		out = append(out, app.apiFilterV2(g))
	}
	apiJSON(w, 200, out)
}

func (app *App) apiFilterGroupOr404(w http.ResponseWriter, c *apiCtx, id string) *FilterGroup {
	g := app.filterGroup(c.Me.ID, id)
	if g == nil {
		apiError(w, 404, "Record not found")
	}
	return g
}

func (app *App) apiCreateFilterV2(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	p := readParams(r)
	g := &FilterGroup{Context: filterContexts}
	readFilterGroup(p, g)
	if err := app.SaveFilterGroup(c.Me, g); err != nil {
		apiError(w, 422, err.Error())
		return
	}
	if err := app.applyKeywordAttrs(c.Me, g, keywordAttrs(p)); err != nil {
		app.DeleteFilterGroup(c.Me, g.ID)
		apiError(w, 422, err.Error())
		return
	}
	apiJSON(w, 200, app.apiFilterV2(app.filterGroup(c.Me.ID, g.ID)))
}

func (app *App) apiUpdateFilterV2(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	g := app.apiFilterGroupOr404(w, c, r.PathValue("id"))
	if g == nil {
		return
	}
	p := readParams(r)
	readFilterGroup(p, g)
	if err := app.SaveFilterGroup(c.Me, g); err != nil {
		apiError(w, 422, err.Error())
		return
	}
	if err := app.applyKeywordAttrs(c.Me, g, keywordAttrs(p)); err != nil {
		apiError(w, 422, err.Error())
		return
	}
	apiJSON(w, 200, app.apiFilterV2(app.filterGroup(c.Me.ID, g.ID)))
}

func (app *App) apiDeleteFilterV2(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if g := app.apiFilterGroupOr404(w, c, r.PathValue("id")); g != nil {
		app.DeleteFilterGroup(c.Me, g.ID)
		apiJSON(w, 200, obj{})
	}
}

// findKeyword returns one of the user's keywords with its group.
func (app *App) findKeyword(me *Account, id string) *Filter {
	for _, g := range app.filterGroups(me.ID) {
		for _, k := range g.Keywords {
			if k.ID == id {
				return k
			}
		}
	}
	return nil
}

// apiFilterSub serves GET /api/v2/filters/{id}/{sub}: a group's keywords or
// statuses, or (as /filters/keywords/{id}) a single keyword. One pattern,
// since Go's mux won't register both overlapping shapes.
func (app *App) apiFilterSub(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	id, sub := r.PathValue("id"), r.PathValue("sub")
	if id == "keywords" {
		if k := app.findKeyword(c.Me, sub); k != nil {
			apiJSON(w, 200, apiFilterKeyword(k))
		} else {
			apiError(w, 404, "Record not found")
		}
		return
	}
	g := app.apiFilterGroupOr404(w, c, id)
	if g == nil {
		return
	}
	switch sub {
	case "keywords":
		out := []any{}
		for _, k := range g.Keywords {
			out = append(out, apiFilterKeyword(k))
		}
		apiJSON(w, 200, out)
	case "statuses":
		apiJSON(w, 200, []any{}) // filtering single posts is not supported
	default:
		apiError(w, 404, "Record not found")
	}
}

func (app *App) apiAddFilterKeyword(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	g := app.apiFilterGroupOr404(w, c, r.PathValue("id"))
	if g == nil {
		return
	}
	p := readParams(r)
	whole := !p.Has("whole_word") || p.Bool("whole_word")
	k, err := app.AddFilterKeyword(c.Me, g.ID, p.Get("keyword"), whole)
	if err != nil {
		apiError(w, 422, err.Error())
		return
	}
	apiJSON(w, 200, apiFilterKeyword(k))
}

func (app *App) apiUpdateFilterKeyword(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	k := app.findKeyword(c.Me, r.PathValue("id"))
	if k == nil {
		apiError(w, 404, "Record not found")
		return
	}
	p := readParams(r)
	if p.Has("keyword") {
		k.Phrase = p.Get("keyword")
	}
	if p.Has("whole_word") {
		k.WholeWord = p.Bool("whole_word")
	}
	if err := app.UpdateFilterKeyword(c.Me, k.ID, k.Phrase, k.WholeWord); err != nil {
		apiError(w, 422, err.Error())
		return
	}
	apiJSON(w, 200, apiFilterKeyword(app.findKeyword(c.Me, k.ID)))
}

func (app *App) apiDeleteFilterKeyword(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if k := app.findKeyword(c.Me, r.PathValue("id")); k != nil {
		app.DeleteFilterKeyword(c.Me, k.ID)
	}
	apiJSON(w, 200, obj{})
}

// ---- v1: one keyword per filter ----

func (app *App) apiFiltersV1(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	for _, k := range app.filtersFor(c.Me.ID) {
		out = append(out, apiFilterV1(k))
	}
	apiJSON(w, 200, out)
}

func (app *App) apiGetFilterV1(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if k := app.findKeyword(c.Me, r.PathValue("id")); k != nil {
		apiJSON(w, 200, apiFilterV1(k))
	} else {
		apiError(w, 404, "Record not found")
	}
}

// readV1 applies phrase, context, irreversible and expires_in to a keyword and its group.
func readV1(p *apiParams, k *Filter) {
	if p.Has("phrase") {
		k.Phrase = p.Get("phrase")
		if len(k.Group.Keywords) <= 1 {
			k.Group.Title = k.Phrase
		}
	}
	if p.Has("whole_word") {
		k.WholeWord = p.Bool("whole_word")
	}
	if p.Has("context") {
		k.Group.Context = p.List("context")
	}
	if p.Has("irreversible") {
		k.Group.Action = map[bool]string{true: "hide", false: "warn"}[p.Bool("irreversible")]
	}
	readExpiry(p, k.Group)
}

func (app *App) apiCreateFilterV1(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	p := readParams(r)
	k := &Filter{WholeWord: true, Group: &FilterGroup{Context: filterContexts}}
	readV1(p, k)
	phrase, err := checkFilterPhrase(k.Phrase)
	if err == nil {
		k.Group.Title = phrase
		err = app.SaveFilterGroup(c.Me, k.Group)
	}
	if err == nil {
		k, err = app.AddFilterKeyword(c.Me, k.Group.ID, phrase, k.WholeWord)
	}
	if err != nil {
		apiError(w, 422, err.Error())
		return
	}
	apiJSON(w, 200, apiFilterV1(app.findKeyword(c.Me, k.ID)))
}

func (app *App) apiUpdateFilterV1(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	k := app.findKeyword(c.Me, r.PathValue("id"))
	if k == nil {
		apiError(w, 404, "Record not found")
		return
	}
	readV1(readParams(r), k)
	err := app.UpdateFilterKeyword(c.Me, k.ID, k.Phrase, k.WholeWord)
	if err == nil {
		err = app.SaveFilterGroup(c.Me, k.Group)
	}
	if err != nil {
		apiError(w, 422, err.Error())
		return
	}
	apiJSON(w, 200, apiFilterV1(app.findKeyword(c.Me, k.ID)))
}

func (app *App) apiDeleteFilterV1(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	app.DeleteFilter(c.Me, r.PathValue("id"))
	apiJSON(w, 200, obj{})
}
