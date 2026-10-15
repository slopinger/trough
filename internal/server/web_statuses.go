package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (app *App) handleThread(w http.ResponseWriter, r *http.Request, p *Page) {
	st, err := app.loadStatus(p.Me, r.PathValue("id"))
	if err != nil {
		app.notFound(w, p)
		return
	}
	app.showThread(w, r, p, st)
}

func (app *App) showThread(w http.ResponseWriter, r *http.Request, p *Page, st *Status) {
	if st.IsBoost() {
		http.Redirect(w, r, st.BoostOf.Path(), http.StatusFound)
		return
	}
	// remote polls: get fresh counts
	if p.Me != nil && !st.Local && st.Poll != nil && !st.Poll.Closed() {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		if app.refreshStatus(ctx, st) == nil {
			if fresh, err := app.loadStatus(p.Me, st.ID); err == nil {
				st = fresh
			}
		}
		cancel()
	}
	// if we are missing the parent, try to fetch it (logged-in users only)
	if p.Me != nil && st.InReplyToURI != "" && st.InReplyToID == "" {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		if parent, err := app.resolveStatus(ctx, st.InReplyToURI); err == nil {
			st.InReplyToID = parent.ID
		}
		cancel()
	}
	p.Status = st
	p.Ancestors, p.Descendants = app.thread(p.Me, st)
	p.Title = st.Account.Name() + ": " + truncateRunes(plainText(st.Content), 60)
	if p.Me != nil {
		p.Compose = app.replyCompose(p.Me, st)
	}
	app.render(w, "thread", p)
}

func (app *App) replyCompose(me *Account, st *Status) *Compose {
	c := &Compose{ReplyTo: st, Visibility: st.Visibility, Spoiler: st.Spoiler}
	var mentions []string
	seen := map[string]bool{me.ID: true}
	add := func(a *Account) {
		if a != nil && !seen[a.ID] {
			seen[a.ID] = true
			mentions = append(mentions, "@"+a.Acct())
		}
	}
	seenAcct := map[string]bool{strings.ToLower(me.Acct()): true}
	addAcct := func(acct string) {
		if k := strings.ToLower(acct); acct != "" && !seenAcct[k] {
			seenAcct[k] = true
			mentions = append(mentions, "@"+acct)
		}
	}
	add = func(a *Account) {
		if a != nil && !seen[a.ID] {
			seen[a.ID] = true
			addAcct(a.Acct())
		}
	}
	add(st.Account)
	for _, m := range st.Mentions {
		add(m)
	}
	// everyone mentioned in the post, including accounts we don't know yet
	for _, l := range app.mentionLinks(st.Content) {
		addAcct(l.Handle)
	}
	if len(mentions) > 0 {
		c.Text = strings.Join(mentions, " ") + " "
	}
	return c
}

func (app *App) handleComposePage(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section = "New post", "compose"
	p.Compose = &Compose{Visibility: "public"}
	if id := r.URL.Query().Get("reply_to"); id != "" {
		if st, err := app.loadStatus(p.Me, id); err == nil {
			if st.IsBoost() {
				st = st.BoostOf
			}
			p.Compose = app.replyCompose(p.Me, st)
		}
	}
	if t := r.URL.Query().Get("text"); t != "" {
		p.Compose.Text = t
	}
	if id := r.URL.Query().Get("quote"); id != "" {
		if st, err := app.loadStatus(p.Me, id); err == nil {
			if st.IsBoost() {
				st = st.BoostOf
			}
			if st.Visibility == "public" || st.Visibility == "unlisted" {
				p.Compose.QuoteOf = st
				p.Title = "Quote post"
			}
		}
	}
	if id := r.URL.Query().Get("edit"); id != "" {
		st, err := app.loadStatus(p.Me, id)
		if err != nil || st.AccountID != p.Me.ID || st.IsBoost() || !st.Local {
			app.notFound(w, p)
			return
		}
		text := st.Source
		if text == "" {
			text = plainText(st.Content)
		}
		p.Title = "Edit post"
		p.Compose = &Compose{EditOf: st, Text: text, Spoiler: st.Spoiler, Sensitive: st.Sensitive, Visibility: st.Visibility}
	}
	app.render(w, "compose", p)
}

func (app *App) handlePost(w http.ResponseWriter, r *http.Request, p *Page) {
	poll, perr := newPoll([]string{r.FormValue("poll0"), r.FormValue("poll1"), r.FormValue("poll2"), r.FormValue("poll3")},
		r.FormValue("poll_duration"), r.FormValue("poll_multiple") != "")
	in := PostInput{
		Poll:       poll,
		Text:       r.FormValue("text"),
		Spoiler:    r.FormValue("spoiler"),
		Visibility: r.FormValue("visibility"),
		Sensitive:  r.FormValue("sensitive") != "",
		InReplyTo:  r.FormValue("in_reply_to"),
		QuoteOf:    r.FormValue("quote_of"),
	}
	fail := func(msg string) {
		p.Title, p.Error = "New post", msg
		p.Compose = &Compose{Text: in.Text, Spoiler: in.Spoiler, Visibility: in.Visibility, Sensitive: in.Sensitive}
		if in.QuoteOf != "" {
			p.Compose.QuoteOf, _ = app.loadStatus(p.Me, in.QuoteOf)
		}
		if in.InReplyTo != "" {
			p.Compose.ReplyTo, _ = app.loadStatus(p.Me, in.InReplyTo)
		}
		app.renderStatus(w, http.StatusBadRequest, "compose", p)
	}
	if perr != nil {
		fail(perr.Error())
		return
	}
	if poll != nil && r.MultipartForm != nil && len(r.MultipartForm.File["media0"])+len(r.MultipartForm.File["media1"]) > 0 {
		fail("a post can have images or a poll, not both")
		return
	}
	if r.MultipartForm != nil {
		for i := 0; i < 4; i++ {
			fhs := r.MultipartForm.File["media"+string(rune('0'+i))]
			if len(fhs) == 0 {
				continue
			}
			f, err := fhs[0].Open()
			if err != nil {
				fail("Could not read upload.")
				return
			}
			data, err := io.ReadAll(io.LimitReader(f, maxUploadSize+1))
			f.Close()
			if err != nil {
				fail("Could not read upload.")
				return
			}
			at, err := app.saveUpload(p.Me, data, r.FormValue("alt"+string(rune('0'+i))))
			if err != nil {
				fail(fhs[0].Filename + ": " + err.Error())
				return
			}
			in.MediaIDs = append(in.MediaIDs, at.ID)
		}
	}
	when := r.FormValue("scheduled_at")
	if when == "" {
		when = r.FormValue("scheduled_local") // no JavaScript: taken as UTC
	}
	if at, ok := parseScheduleTime(when); ok {
		in.PollStart = time.Now()
		if _, err := app.Schedule(p.Me, in, at); err != nil {
			fail(err.Error())
			return
		}
		setFlash(w, "Post scheduled for "+at.UTC().Format("Mon, 2 Jan 15:04 MST")+".")
		http.Redirect(w, r, "/scheduled", http.StatusSeeOther)
		return
	}
	st, err := app.PostStatus(r.Context(), p.Me, in)
	if err != nil {
		fail(err.Error())
		return
	}
	if in.InReplyTo != "" {
		http.Redirect(w, r, "/s/"+in.InReplyTo+"#s-"+st.ID, http.StatusSeeOther)
		return
	}
	app.back(w, r, "/")
}

func (app *App) handleStatusAction(w http.ResponseWriter, r *http.Request, p *Page) {
	st, err := app.loadStatus(p.Me, r.PathValue("id"))
	if err != nil {
		http.Error(w, "post not found", http.StatusNotFound)
		return
	}
	target := st
	if st.IsBoost() {
		target = st.BoostOf
	}
	action := r.PathValue("action")
	switch action {
	case "like":
		err = app.Like(p.Me, target)
	case "unlike":
		err = app.Unlike(p.Me, target)
	case "boost":
		err = app.Boost(p.Me, target)
	case "unboost":
		err = app.Unboost(p.Me, target)
	case "vote":
		var choices []int
		for _, c := range r.Form["choice"] {
			if n, cerr := strconv.Atoi(c); cerr == nil {
				choices = append(choices, n)
			}
		}
		err = app.Vote(p.Me, target, choices)
	case "react":
		err = app.React(p.Me, target, r.FormValue("emoji"))
	case "unreact":
		err = app.Unreact(p.Me, target, r.FormValue("emoji"))
	case "bookmark":
		err = app.Bookmark(p.Me, target)
	case "unbookmark":
		err = app.Unbookmark(p.Me, target)
	case "pin":
		err = app.Pin(p.Me, target)
	case "unpin":
		err = app.Unpin(p.Me, target)
	case "edit":
		var edited *Status
		edited, err = app.EditStatus(r.Context(), p.Me, target, r.FormValue("text"), r.FormValue("spoiler"), r.FormValue("sensitive") != "")
		if err == nil && !isFetch(r) {
			http.Redirect(w, r, edited.Path(), http.StatusSeeOther)
			return
		}
		if err != nil && !isFetch(r) {
			p.Title, p.Error = "Edit post", err.Error()
			p.Compose = &Compose{EditOf: target, Text: r.FormValue("text"), Spoiler: r.FormValue("spoiler"), Visibility: target.Visibility}
			app.renderStatus(w, http.StatusBadRequest, "compose", p)
			return
		}
	case "delete":
		err = app.DeleteStatus(p.Me, st)
		if err == nil && !isFetch(r) {
			setFlash(w, "Post deleted.")
			if ref, perr := url.Parse(r.Referer()); perr == nil && strings.HasPrefix(ref.Path, "/s/"+st.ID) {
				http.Redirect(w, r, p.Me.Path(), http.StatusSeeOther)
				return
			}
		}
	default:
		http.NotFound(w, r)
		return
	}
	if isFetch(r) {
		if err != nil {
			writeJSON(w, "application/json", obj{"error": err.Error()})
			return
		}
		fresh, _ := app.loadStatus(p.Me, target.ID)
		res := obj{"ok": true}
		if fresh != nil {
			res["liked"], res["boosted"], res["likes"], res["boosts"] = fresh.Liked, fresh.Boosted, fresh.Likes, fresh.Boosts
			res["bookmarked"], res["pinned"] = fresh.Bookmarked, !fresh.PinnedAt.IsZero()
		}
		writeJSON(w, "application/json", res)
		return
	}
	if err != nil {
		setFlash(w, err.Error())
	}
	app.back(w, r, "/")
}

func (app *App) handleReportForm(w http.ResponseWriter, r *http.Request, p *Page) {
	acc, err := app.AccountByID(r.URL.Query().Get("account"))
	if err != nil || acc.ID == p.Me.ID {
		app.notFound(w, p)
		return
	}
	p.Title, p.Account = "Report @"+acc.Acct(), acc
	sts, _ := app.accountTimeline(p.Me, acc, true, false, "")
	if id := r.URL.Query().Get("status"); id != "" {
		if st, err := app.loadStatus(p.Me, id); err == nil && st.AccountID == acc.ID {
			p.Status = st
		}
	}
	p.Statuses = sts
	app.render(w, "report", p)
}

func (app *App) handleReport(w http.ResponseWriter, r *http.Request, p *Page) {
	acc, err := app.AccountByID(r.FormValue("account"))
	if err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	if _, err := app.FileReport(p.Me, acc, r.Form["status"], r.FormValue("comment"), r.FormValue("forward") != ""); err != nil {
		setFlash(w, err.Error())
	} else {
		setFlash(w, "Thanks, the report was sent to the admins"+map[bool]string{true: " of this server and " + acc.Domain, false: ""}[r.FormValue("forward") != "" && !acc.IsLocal()]+".")
	}
	http.Redirect(w, r, acc.Path(), http.StatusSeeOther)
}

func (app *App) handleHistory(w http.ResponseWriter, r *http.Request, p *Page) {
	st, err := app.loadStatus(p.Me, r.PathValue("id"))
	if err != nil || st.IsBoost() {
		app.notFound(w, p)
		return
	}
	p.Title, p.Status = "Edit history", st
	p.Versions = app.history(st)
	app.render(w, "history", p)
}
