package server

import (
	"context"
	"net/http"
	"strings"
	"time"
)

func (app *App) handleProfileOrStatus(w http.ResponseWriter, r *http.Request, p *Page) {
	at := r.PathValue("at")
	if name, ok := strings.CutSuffix(at, ".rss"); ok && r.PathValue("id") == "" && strings.HasPrefix(name, "@") {
		if acc, err := app.LocalAccount(strings.TrimPrefix(name, "@")); err == nil && !acc.Suspended {
			app.handleProfileRSS(w, r, acc)
			return
		}
	}
	if !strings.HasPrefix(at, "@") {
		app.notFound(w, p)
		return
	}
	user, domain, ok := parseHandle(at)
	if !ok {
		app.notFound(w, p)
		return
	}
	var acc *Account
	var err error
	if domain == "" || domain == app.cfg.Host {
		acc, err = app.LocalAccount(user)
	} else {
		acc, err = app.AccountByAcct(user, domain)
		if err != nil && p.Me != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			acc, err = app.resolveAcct(ctx, user, domain)
			cancel()
		}
	}
	if err != nil || acc.Suspended {
		app.notFound(w, p)
		return
	}
	sub := r.PathValue("id")
	if acc.IsLocal() && wantsAP(r) {
		if sub == "" {
			writeAP(w, app.actorJSON(acc))
		} else {
			app.apStatus(w, r, acc.Username, sub, false)
		}
		return
	}
	if r.PathValue("tag") != "" {
		if sub != "tagged" {
			app.notFound(w, p)
			return
		}
		sub = ""
	}
	switch sub {
	case "":
		app.showProfile(w, r, p, acc)
	case "followers", "following":
		app.showFollows(w, r, p, acc, sub == "followers")
	default:
		st, err := app.loadStatus(p.Me, sub)
		if err != nil || st.AccountID != acc.ID {
			app.notFound(w, p)
			return
		}
		app.showThread(w, r, p, st)
	}
}

func (app *App) profileCommon(p *Page, acc *Account) {
	p.Account = acc
	if acc.MovedTo != "" {
		p.MovedTo, _ = app.AccountByURI(acc.MovedTo)
	}
	if p.Me != nil {
		p.Rel = app.relationship(p.Me.ID, acc.ID)
		p.Rel.DomainBlocking = app.blocksDomain(p.Me.ID, acc.Domain)
		if acc.ID != p.Me.ID {
			p.Familiar = app.familiarFollowers(p.Me, acc, 3)
		}
		p.AccountNote = app.accountNote(p.Me, acc)
	}
	if acc.IsLocal() {
		p.Counts.Posts = app.countInt(`SELECT COUNT(*) FROM statuses WHERE account_id=? AND boost_of_id=''`, acc.ID)
	} else {
		p.Counts.Posts = app.countInt(`SELECT COUNT(*) FROM statuses WHERE account_id=? AND boost_of_id='' AND visibility IN ('public','unlisted')`, acc.ID)
	}
	if acc.IsLocal() {
		p.Featured, _ = app.endorsements(acc.ID)
	}
	p.Counts.Following = app.countInt(`SELECT COUNT(*) FROM follows WHERE account_id=? AND accepted=1`, acc.ID)
	p.Counts.Followers = app.countInt(`SELECT COUNT(*) FROM follows WHERE target_id=? AND accepted=1`, acc.ID)
}

func (app *App) showProfile(w http.ResponseWriter, r *http.Request, p *Page, acc *Account) {
	if p.Me != nil && !acc.IsLocal() && r.URL.Query().Get("max_id") == "" &&
		app.countInt(`SELECT COUNT(*) FROM statuses WHERE account_id=?`, acc.ID) < 5 {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		app.backfillOutbox(ctx, acc)
		cancel()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			app.syncFeatured(ctx, acc)
		}()
	}
	app.profileCommon(p, acc)
	p.Tab = r.URL.Query().Get("tab")
	p.FeaturedTags = app.featuredTags(acc)
	if p.Tab == "featured" {
		p.Title = "Featured by " + acc.Name() + " (@" + acc.Username + "@" + app.accountHost(acc) + ")"
		if p.Me != nil && p.Me.ID == acc.ID {
			p.Section = "profile"
		}
		app.render(w, "profile", p)
		return
	}
	var sts []*Status
	var err error
	if tag := normalizeTag(r.PathValue("tag")); tag != "" { // /@name/tagged/tag
		p.Tag, p.Tab = tag, "tagged"
		sts, err = app.queryStatuses(p.Me, `s.account_id=? AND s.id IN (SELECT status_id FROM status_tags WHERE tag=?)`,
			[]any{acc.ID, tag}, r.URL.Query().Get("max_id"), pageSize, false)
	} else {
		sts, err = app.accountTimeline(p.Me, acc, p.Tab == "replies", p.Tab == "media", r.URL.Query().Get("max_id"))
	}
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Statuses = sts
	if p.Tab == "" && r.URL.Query().Get("max_id") == "" {
		p.Pinned = app.pinnedStatuses(p.Me, acc)
		pinned := map[string]bool{}
		for _, st := range p.Pinned {
			pinned[st.ID] = true
		}
		rest := sts[:0:0]
		for _, st := range sts {
			if !pinned[st.ID] {
				rest = append(rest, st)
			}
		}
		p.Statuses = rest
	}
	p.NextURL = nextURL(r, sts)
	p.Title = acc.Name() + " (@" + acc.Username + "@" + app.accountHost(acc) + ")"
	if p.Me != nil && p.Me.ID == acc.ID {
		p.Section = "profile"
	}
	if p.Me != nil && p.Rel.Following {
		p.Lists = app.lists(p.Me)
	}
	app.render(w, "profile", p)
}

func (app *App) showFollows(w http.ResponseWriter, r *http.Request, p *Page, acc *Account, followers bool) {
	app.profileCommon(p, acc)
	list, err := app.followList(acc, followers, false, 0)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	p.Accounts = list
	if followers {
		p.Tab, p.Title = "followers", "Followers of "+acc.Name()
	} else {
		p.Tab, p.Title = "following", acc.Name()+" follows"
	}
	app.render(w, "follows", p)
}

func (app *App) handleAccountAction(w http.ResponseWriter, r *http.Request, p *Page) {
	target, err := app.AccountByID(r.PathValue("id"))
	if err != nil || target.ActorType == "Application" && target.IsLocal() {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	switch r.PathValue("action") {
	case "follow":
		err = app.Follow(p.Me, target)
		if err == nil && !target.IsLocal() {
			setFlash(w, "Follow request sent to @"+target.Acct()+".")
		}
	case "unfollow":
		err = app.Unfollow(p.Me, target)
	case "block":
		err = app.Block(p.Me, target)
	case "unblock":
		err = app.Unblock(p.Me, target)
	case "mute":
		err = app.Mute(p.Me, target)
	case "unmute":
		err = app.Unmute(p.Me, target)
	case "note":
		err = app.SetAccountNote(p.Me, target, r.FormValue("comment"))
	case "endorse":
		err = app.Endorse(p.Me, target)
	case "unendorse":
		err = app.Unendorse(p.Me, target)
	case "dismiss_suggestion":
		app.dismissSuggestion(p.Me, target.ID)
	case "accept":
		err = app.AcceptFollow(p.Me, target)
	case "reject", "remove_follower":
		err = app.RejectFollow(p.Me, target)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		setFlash(w, err.Error())
	}
	app.back(w, r, target.Path())
}
