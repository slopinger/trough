package server

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (app *App) handleSettings(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section = "Settings", "settings"
	p.Account = p.Me
	p.Filters = app.filtersFor(p.Me.ID)
	p.Apps = app.connectedApps(p.Me.ID)
	p.FollowedTags = app.followedTags(p.Me)
	p.QuotePolicy = app.defaultQuotePolicy(p.Me.ID)
	for _, uri := range p.Me.AlsoKnownAs {
		if a, err := app.AccountByURI(uri); err == nil {
			p.Aliases = append(p.Aliases, a)
		} else {
			p.Aliases = append(p.Aliases, &Account{URI: uri, Username: uri})
		}
	}
	if p.Me.MovedTo != "" {
		p.MovedTo, _ = app.AccountByURI(p.Me.MovedTo)
	}
	p.Accounts, _ = app.accountsQuery(`SELECT `+accountCols+` FROM mutes m JOIN accounts a ON a.id=m.target_id WHERE m.account_id=? ORDER BY m.created_at DESC`, p.Me.ID)
	p.BlockedDomains = app.userDomainBlocks(p.Me)
	p.Import = importStatus(p.Me.ID)
	p.TwoFactor = app.twoFactorEnabled(p.Me.ID)
	p.NotifPolicy = app.notificationPolicy(p.Me.ID)
	p.VapidKey = app.vapidPublic()
	p.Blocked, _ = app.accountsQuery(`SELECT `+accountCols+` FROM blocks b JOIN accounts a ON a.id=b.target_id WHERE b.account_id=? ORDER BY b.created_at DESC`, p.Me.ID)
	app.render(w, "settings", p)
}

func (app *App) readImageField(r *http.Request, field string) ([]byte, bool) {
	if r.MultipartForm == nil {
		return nil, false
	}
	fhs := r.MultipartForm.File[field]
	if len(fhs) == 0 || fhs[0].Size == 0 {
		return nil, false
	}
	f, err := fhs[0].Open()
	if err != nil {
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUploadSize+1))
	return data, err == nil
}

func (app *App) replaceImage(me *Account, old string, data []byte) (string, error) {
	at, err := app.saveUpload(me, data, "")
	if err != nil {
		return "", err
	}
	app.db.Exec(`DELETE FROM attachments WHERE id=?`, at.ID) // not a post attachment
	if strings.HasPrefix(old, app.cfg.BaseURL()+"/media/") {
		os.Remove(filepath.Join(app.mediaDir, filepath.Base(old)))
	}
	return at.URL, nil
}

func (app *App) handleSettingsProfile(w http.ResponseWriter, r *http.Request, p *Page) {
	me := p.Me
	me.DisplayName = truncateRunes(strings.TrimSpace(r.FormValue("display_name")), 100)
	me.NoteSource = truncateRunes(strings.TrimSpace(r.FormValue("note")), 2000)
	me.Note = app.formatBio(r.Context(), me.NoteSource)
	me.Locked = r.FormValue("locked") != ""
	me.Discoverable = r.FormValue("discoverable") != ""
	if r.FormValue("bot") != "" {
		me.ActorType = "Service"
	} else {
		me.ActorType = "Person"
	}
	me.Fields = nil
	for i := 0; i < 4; i++ {
		n := truncateRunes(strings.TrimSpace(r.FormValue("field_name"+string(rune('0'+i)))), 100)
		v := truncateRunes(strings.TrimSpace(r.FormValue("field_value"+string(rune('0'+i)))), 500)
		if n != "" || v != "" {
			me.Fields = append(me.Fields, Field{Name: n, Value: fieldHTML(v)})
		}
	}
	var errs []string
	if qp := r.FormValue("quote_policy"); qp != "" {
		if err := app.SetDefaultQuotePolicy(me, qp); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if domains, err := attributionDomains(strings.Fields(r.FormValue("attribution_domains"))); err == nil {
		me.AttributionDomains = domains
	} else {
		errs = append(errs, err.Error())
	}
	if data, ok := app.readImageField(r, "avatar"); ok {
		if u, err := app.replaceImage(me, me.Avatar, data); err == nil {
			me.Avatar = u
		} else {
			errs = append(errs, "avatar: "+err.Error())
		}
	} else if r.FormValue("remove_avatar") != "" {
		app.replaceRemove(me.Avatar)
		me.Avatar = ""
	}
	if data, ok := app.readImageField(r, "header"); ok {
		if u, err := app.replaceImage(me, me.Header, data); err == nil {
			me.Header = u
		} else {
			errs = append(errs, "header: "+err.Error())
		}
	} else if r.FormValue("remove_header") != "" {
		app.replaceRemove(me.Header)
		me.Header = ""
	}
	if err := app.UpdateProfile(me); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		setFlash(w, "Saved, but: "+strings.Join(errs, "; "))
	} else {
		setFlash(w, "Profile saved.")
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (app *App) replaceRemove(old string) {
	if strings.HasPrefix(old, app.cfg.BaseURL()+"/media/") {
		os.Remove(filepath.Join(app.mediaDir, filepath.Base(old)))
	}
}

// fieldHTML renders a profile field value: a bare URL becomes a link.
func fieldHTML(v string) string {
	if (strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://")) && !strings.ContainsAny(v, " \t\n") {
		return linkHTML(v)
	}
	return htmlEscape(v)
}

func (app *App) handleSettingsPassword(w http.ResponseWriter, r *http.Request, p *Page) {
	if _, err := app.checkPassword(p.Me.Username, r.FormValue("current")); err != nil {
		setFlash(w, "Current password is wrong.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if r.FormValue("new") != r.FormValue("confirm") {
		setFlash(w, "The new passwords don't match.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if err := app.SetPassword(p.Me.ID, r.FormValue("new")); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	setFlash(w, "Password changed. Please log in again.")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (app *App) handleAddFilter(w http.ResponseWriter, r *http.Request, p *Page) {
	if err := app.AddFilter(p.Me, r.FormValue("phrase"), r.FormValue("whole_word") != "", r.FormValue("action")); err != nil {
		setFlash(w, err.Error())
	} else {
		setFlash(w, "Filter added.")
	}
	http.Redirect(w, r, "/settings#filters", http.StatusSeeOther)
}

func (app *App) handleDeleteFilter(w http.ResponseWriter, r *http.Request, p *Page) {
	app.DeleteFilter(p.Me, r.PathValue("id"))
	http.Redirect(w, r, "/settings#filters", http.StatusSeeOther)
}

func (app *App) handleSettingsAccount(w http.ResponseWriter, r *http.Request, p *Page) {
	var err error
	msg := ""
	switch r.FormValue("action") {
	case "add_alias":
		err = app.AddAlias(r.Context(), p.Me, r.FormValue("handle"))
		msg = "Alias added."
	case "remove_alias":
		err = app.RemoveAlias(p.Me, r.FormValue("uri"))
		msg = "Alias removed."
	case "move":
		if _, cerr := app.checkPassword(p.Me.Username, r.FormValue("password")); cerr != nil {
			err = errors.New("wrong password")
			break
		}
		var target *Account
		target, err = app.MoveTo(r.Context(), p.Me, r.FormValue("handle"))
		if err == nil {
			msg = "Your followers are being moved to @" + target.Acct() + "."
		}
	case "cancel_move":
		p.Me.MovedTo = ""
		err = app.UpdateProfile(p.Me)
		msg = "This account is no longer marked as moved."
	}
	if err != nil {
		msg = err.Error()
	}
	setFlash(w, msg)
	http.Redirect(w, r, "/settings#account", http.StatusSeeOther)
}

func (app *App) handleRevokeApp(w http.ResponseWriter, r *http.Request, p *Page) {
	app.db.Exec(`DELETE FROM oauth_tokens WHERE id=? AND account_id=?`, r.PathValue("id"), p.Me.ID)
	setFlash(w, "Access revoked.")
	http.Redirect(w, r, "/settings#apps", http.StatusSeeOther)
}
