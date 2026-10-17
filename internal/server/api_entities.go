package server

// Mastodon API entities.

import (
	"fmt"
	"strings"
	"time"
)

func (f Field) verifiedAt() any {
	if f.VerifiedAt == 0 {
		return nil
	}
	return apiTime(time.Unix(f.VerifiedAt, 0))
}

func apiTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// absMedia returns an absolute URL clients can load (local or proxied).
func (app *App) absMedia(u string) string {
	m := app.mediaURL(u)
	if m == "" {
		return ""
	}
	if strings.HasPrefix(m, "/") {
		return app.cfg.BaseURL() + m
	}
	return m
}

func (app *App) defaultAvatar() string {
	return app.cfg.BaseURL() + "/static/avatar.svg"
}

func (app *App) apiEmojis(list []Emoji) []any {
	out := []any{}
	for _, e := range list {
		u := app.absMedia(e.URL)
		if u == "" {
			continue
		}
		out = append(out, obj{"shortcode": e.Shortcode, "url": u, "static_url": u, "visible_in_picker": false})
	}
	return out
}

func (app *App) apiAccount(a *Account) obj {
	return app.apiAccountDepth(a, 0)
}

func (app *App) apiAccountDepth(a *Account, depth int) obj {
	avatar := app.absMedia(a.Avatar)
	if avatar == "" {
		avatar = app.defaultAvatar()
	}
	header := app.absMedia(a.Header)
	if header == "" {
		header = app.cfg.BaseURL() + "/static/favicon.svg"
	}
	fields := []any{}
	for _, f := range a.Fields {
		fields = append(fields, obj{"name": f.Name, "value": f.Value, "verified_at": f.verifiedAt()})
	}
	statuses := app.countInt(`SELECT COUNT(*) FROM statuses WHERE account_id=?`, a.ID)
	var lastStatus any
	var last int64
	if app.db.QueryRow(`SELECT MAX(created_at) FROM statuses WHERE account_id=?`, a.ID).Scan(&last) == nil && last > 0 {
		lastStatus = time.Unix(last, 0).UTC().Format("2006-01-02")
	}
	url := a.URL
	if url == "" {
		url = a.URI
	}
	m := obj{
		"id":               a.ID,
		"username":         a.Username,
		"acct":             a.Acct(),
		"display_name":     a.DisplayName,
		"locked":           a.Locked,
		"bot":              a.IsBot(),
		"discoverable":     a.Discoverable,
		"hide_collections": a.HideCollections,
		"indexable":        a.Indexable,
		"group":            a.ActorType == "Group",
		"created_at":       apiTime(a.CreatedAt.UTC().Truncate(24 * time.Hour)),
		"note":             a.Note,
		"url":              url,
		"uri":              a.URI,
		"avatar":           avatar,
		"avatar_static":    avatar,
		"header":           header,
		"header_static":    header,
		"followers_count":  app.countInt(`SELECT COUNT(*) FROM follows WHERE target_id=? AND accepted=1`, a.ID),
		"following_count":  app.countInt(`SELECT COUNT(*) FROM follows WHERE account_id=? AND accepted=1`, a.ID),
		"statuses_count":   statuses,
		"last_status_at":   lastStatus,
		"noindex":          a.NoIndex,
		"limited":          a.IsLimited(),
		"emojis":           app.apiEmojis(a.Emojis),
		"fields":           fields,
		"roles":            []any{},
	}
	if a.MovedTo != "" && depth == 0 {
		if mv, err := app.AccountByURI(a.MovedTo); err == nil {
			m["moved"] = app.apiAccountDepth(mv, 1)
		}
	}
	return m
}

func (app *App) apiCredentialAccount(a *Account) obj {
	m := app.apiAccount(a)
	fields := []any{}
	for _, f := range a.Fields {
		fields = append(fields, obj{"name": f.Name, "value": plainText(f.Value), "verified_at": f.verifiedAt()})
	}
	pr := app.prefs(a.ID)
	m["source"] = obj{"privacy": pr.Visibility, "sensitive": pr.Sensitive, "language": pr.Language, "note": a.NoteSource, "fields": fields,
		"follow_requests_count": app.followRequestCount(a.ID), "quote_policy": app.defaultQuotePolicy(a.ID),
		"attribution_domains": nonNil(a.AttributionDomains), "discoverable": a.Discoverable}
	m["role"] = obj{"id": "0", "name": map[bool]string{true: "Admin", false: ""}[app.isAdmin(a.ID)], "permissions": "0", "color": "", "highlighted": false}
	return m
}

func (app *App) apiMedia(at *Attachment) obj {
	kind := at.Kind()
	if kind == "image" && at.MediaType == "image/gif" {
		kind = "image"
	}
	u := app.absMedia(at.URL)
	var preview any = u // videos and audio: their poster image, if any
	if kind != "image" {
		preview = nullIfEmpty(app.absMedia(at.PreviewURL))
	}
	m := obj{
		"id": at.ID, "type": kind, "url": u, "preview_url": preview, "text_url": nil,
		"remote_url": nil, "description": nullIfEmpty(at.Description), "blurhash": nil, "meta": obj{},
	}
	if !strings.HasPrefix(at.URL, app.cfg.BaseURL()) {
		m["remote_url"] = at.URL
	}
	if at.PreviewURL != "" && !strings.HasPrefix(at.PreviewURL, app.cfg.BaseURL()) {
		m["preview_remote_url"] = at.PreviewURL
	}
	if at.Width > 0 && at.Height > 0 {
		orig := obj{"width": at.Width, "height": at.Height, "size": fmt.Sprintf("%dx%d", at.Width, at.Height), "aspect": float64(at.Width) / float64(at.Height)}
		m["meta"] = obj{"original": orig, "small": orig}
	}
	if at.HasFocus() {
		meta, _ := m["meta"].(obj)
		meta["focus"] = obj{"x": at.FocusX, "y": at.FocusY}
		m["meta"] = meta
	}
	return m
}

func (app *App) apiPoll(s *Status) any {
	p := s.Poll
	if p == nil {
		return nil
	}
	opts := []any{}
	for _, o := range p.Options {
		opts = append(opts, obj{"title": o.Name, "votes_count": o.Votes})
	}
	own := []int{}
	for i := range p.Options {
		if s.MyVotes[i] {
			own = append(own, i)
		}
	}
	var voters any
	if s.Local {
		voters = app.countInt(`SELECT COUNT(DISTINCT account_id) FROM poll_votes WHERE status_id=?`, s.ID)
	}
	return obj{"id": s.ID, "expires_at": apiTime(p.EndTime), "expired": p.Closed(), "multiple": p.Multiple,
		"votes_count": p.Total(), "voters_count": voters, "options": opts, "emojis": []any{},
		"voted": len(own) > 0, "own_votes": own}
}

func (app *App) apiCard(c *LinkCard) any {
	if c == nil {
		return nil
	}
	return obj{"url": c.URL, "title": c.Title, "description": c.Description, "type": "link", "author_name": "", "author_url": "",
		"provider_name": c.Site, "provider_url": "", "html": "", "width": 0, "height": 0, "image": nullIfEmpty(app.absMedia(c.Image)),
		"embed_url": "", "blurhash": nil, "language": nil, "published_at": nil, "authors": []any{}}
}

func (app *App) apiStatus(v *Account, s *Status) obj {
	if s.IsBoost() && s.BoostOf != nil {
		inner := app.apiStatusBase(v, s.BoostOf)
		m := app.apiStatusBase(v, s)
		m["reblog"] = inner
		m["content"], m["media_attachments"], m["mentions"], m["tags"], m["poll"], m["card"] = "", []any{}, []any{}, []any{}, nil, nil
		m["reblogged"], m["favourited"], m["bookmarked"] = inner["reblogged"], inner["favourited"], inner["bookmarked"]
		return m
	}
	return app.apiStatusBase(v, s)
}

func (app *App) apiStatusBase(v *Account, s *Status) obj {
	media := []any{}
	for _, at := range s.Attachments {
		media = append(media, app.apiMedia(at))
	}
	mentions := []any{}
	for _, a := range s.Mentions {
		url := a.URL
		if url == "" {
			url = a.URI
		}
		mentions = append(mentions, obj{"id": a.ID, "username": a.Username, "url": url, "acct": a.Acct()})
	}
	tags := []any{}
	for _, t := range s.Tags {
		tags = append(tags, obj{"name": t, "url": app.cfg.BaseURL() + "/tags/" + t})
	}
	var inReplyTo, inReplyToAcc any
	if s.InReplyToID != "" {
		inReplyTo = s.InReplyToID
	}
	if s.InReplyToAccountID != "" {
		inReplyToAcc = s.InReplyToAccountID
	}
	url := s.URL
	if url == "" {
		url = s.URI
	}
	var app_ any
	if s.Local {
		app_ = obj{"name": SoftwareName, "website": nil}
	}
	filtered := app.apiFilterResult(s)
	reactions := []any{}
	for _, r := range s.Reactions {
		reactions = append(reactions, obj{"name": strings.Trim(r.Emoji, ":"), "count": r.Count, "me": r.Mine, "url": nullIfEmpty(app.absMedia(r.URL))})
	}
	m := obj{
		"id":                     s.ID,
		"created_at":             apiTime(s.CreatedAt),
		"in_reply_to_id":         inReplyTo,
		"in_reply_to_account_id": inReplyToAcc,
		"sensitive":              s.Sensitive,
		"spoiler_text":           s.Spoiler,
		"visibility":             s.Visibility,
		"language":               nullIfEmpty(s.Language),
		"uri":                    s.URI,
		"url":                    url,
		"replies_count":          s.Replies,
		"reblogs_count":          s.Boosts,
		"favourites_count":       s.Likes,
		"quotes_count":           app.countInt(`SELECT COUNT(*) FROM statuses WHERE quote_uri=? AND quote_state='' AND boost_of_id=''`, s.URI),
		"edited_at":              apiTime(s.EditedAt),
		"favourited":             s.Liked,
		"reblogged":              s.Boosted,
		"muted":                  app.conversationMuted(v, s.ID),
		"bookmarked":             s.Bookmarked,
		"content":                s.Content,
		"filtered":               filtered,
		"reblog":                 nil,
		"application":            app_,
		"account":                app.apiAccount(s.Account),
		"media_attachments":      media,
		"mentions":               mentions,
		"tags":                   tags,
		"emojis":                 app.apiEmojis(s.Emojis),
		"card":                   app.apiCard(s.Card),
		"poll":                   app.apiPoll(s),
		"pleroma":                obj{"emoji_reactions": reactions},
	}
	if v != nil && s.AccountID == v.ID {
		m["pinned"] = !s.PinnedAt.IsZero()
	}
	switch {
	case s.QuoteURI != "" && s.QuoteState != "":
		m["quote"] = obj{"state": s.QuoteState, "quoted_status": nil}
	case s.Quote != nil:
		m["quote"] = obj{"state": "accepted", "quoted_status": app.apiStatusBase(v, s.Quote)}
	case s.QuoteURI != "":
		m["quote"] = obj{"state": "deleted", "quoted_status": nil}
	}
	auto := []any{}
	if p := quotePolicy(s); p == "public" || p == "followers" {
		auto = append(auto, p)
	}
	m["quote_approval"] = obj{"automatic": auto, "manual": []any{}, "current_user": app.quoteApproval(s, v)}
	return m
}

func (app *App) apiStatuses(v *Account, sts []*Status) []any {
	out := []any{}
	for _, s := range sts {
		out = append(out, app.apiStatus(v, s))
	}
	return out
}

var notificationTypes = map[string]string{
	"favourite": "favourite", "reblog": "reblog", "mention": "mention", "follow": "follow", "follow_request": "follow_request",
	"poll": "poll", "quote": "quote", "reaction": "pleroma:emoji_reaction", "signup": "admin.sign_up", "move": "move",
	"update": "update", "quoted_update": "quoted_update", "status": "status", "moderation_warning": "moderation_warning",
	"severed_relationships": "severed_relationships",
}

func (app *App) apiNotification(v *Account, n *Notification) obj {
	m := obj{"id": n.ID, "type": notificationTypes[n.Type], "created_at": apiTime(n.CreatedAt), "account": app.apiAccount(n.From),
		"group_key": groupKey(n, defaultGroupedTypes)}
	if n.Status != nil {
		m["status"] = app.apiStatus(v, n.Status)
	}
	if n.Type == "reaction" && n.Status != nil {
		for _, r := range n.Status.Reactions {
			m["emoji"] = r.Emoji
			break
		}
	}
	if n.Warning != nil {
		m["moderation_warning"] = app.apiWarning(n.Warning)
	}
	if n.Event != nil {
		m["event"] = app.apiSeverance(n.Event)
	}
	return m
}

func (app *App) apiRelationship(me *Account, other *Account) obj {
	r := app.relationship(me.ID, other.ID)
	return obj{
		"id": other.ID, "following": r.Following, "showing_reblogs": (r.Following || r.Requested) && r.ShowingReblogs, "notifying": r.Notifying, "languages": nil,
		"followed_by": r.FollowedBy, "blocking": r.Blocking, "blocked_by": r.BlockedBy, "muting": r.Muting,
		"muting_notifications": r.MutingNotifications, "requested": r.Requested, "requested_by": r.RequestedBy,
		"domain_blocking": app.blocksDomain(me.ID, other.Domain), "endorsed": r.Endorsed, "note": app.accountNote(me, other),
	}
}

func (app *App) apiInstanceV1() obj {
	users := app.countInt(`SELECT COUNT(*) FROM users`)
	var contact any
	if admins, _ := app.accountsQuery(`SELECT ` + accountCols + ` FROM accounts a JOIN users u ON u.account_id=a.id WHERE u.admin=1 ORDER BY u.created_at LIMIT 1`); len(admins) > 0 {
		contact = app.apiAccount(admins[0])
	}
	return obj{
		"uri": app.cfg.Host, "title": app.instanceTitle(), "short_description": app.getSetting("description"),
		"description": app.getSetting("description"), "email": "", "version": mastodonVersion,
		"urls":          obj{"streaming_api": strings.Replace(app.cfg.BaseURL(), "http", "ws", 1)},
		"stats":         obj{"user_count": users, "status_count": app.countInt(`SELECT COUNT(*) FROM statuses WHERE local=1 AND boost_of_id=''`), "domain_count": app.countInt(`SELECT COUNT(DISTINCT domain) FROM accounts WHERE domain != ''`)},
		"thumbnail":     app.thumbnailURL(),
		"languages":     []string{"en"},
		"registrations": app.registrationMode() == "open" || app.registrationMode() == "approval", "approval_required": app.registrationMode() == "approval", "invites_enabled": app.registrationMode() == "invite",
		"configuration": app.apiConfiguration(), "contact_account": contact, "rules": app.apiRuleList(),
	}
}

// mastodonVersion and api_versions.mastodon (7) tell clients which
// Mastodon API trough speaks: everything the Mastodon docs list up to API
// version 7 (4.5: quote posts and their policies, featured accounts,
// attribution domains, blur filters, media deletion) is implemented.
const mastodonVersion = "4.5.0 (compatible; trough " + SoftwareVersion + ")"

func (app *App) apiConfiguration() obj {
	return obj{
		"urls": obj{"streaming": strings.Replace(app.cfg.BaseURL(), "http", "ws", 1), "status": nil, "about": app.cfg.BaseURL() + "/about",
			"privacy_policy": app.cfg.BaseURL() + "/privacy-policy", "terms_of_service": app.termsURL()},
		"accounts": obj{"max_featured_tags": 0, "max_pinned_statuses": maxPins},
		"statuses": obj{"max_characters": maxStatusChars, "max_media_attachments": 4, "characters_reserved_per_url": 23},
		"media_attachments": obj{
			"supported_mime_types": []string{"image/jpeg", "image/png", "image/gif", "image/webp", "video/mp4", "video/quicktime", "video/webm",
				"audio/mpeg", "audio/mp4", "audio/ogg", "audio/wav", "audio/flac", "audio/webm"},
			"image_size_limit": maxUploadSize, "image_matrix_limit": maxImagePixels,
			"video_size_limit": maxMediaSize, "video_frame_rate_limit": 120, "video_matrix_limit": 16384 * 16384, "description_limit": 1500,
		},
		"polls":            obj{"max_options": 4, "max_characters_per_option": 100, "min_expiration": 300, "max_expiration": 7 * 24 * 3600},
		"translation":      obj{"enabled": app.aiEnabled()},
		"vapid":            obj{"public_key": app.vapidPublic()},
		"reactions":        obj{"max_reactions": 1},
		"timelines_access": obj{"live_feeds": obj{"local": "public", "remote": "authenticated"}},
	}
}

func (app *App) apiInstanceV2() obj {
	var contact any
	if admins, _ := app.accountsQuery(`SELECT ` + accountCols + ` FROM accounts a JOIN users u ON u.account_id=a.id WHERE u.admin=1 ORDER BY u.created_at LIMIT 1`); len(admins) > 0 {
		contact = app.apiAccount(admins[0])
	}
	return obj{
		"domain": app.cfg.Host, "title": app.instanceTitle(), "version": mastodonVersion,
		"source_url": SourceURL, "description": app.getSetting("description"),
		"usage":     obj{"users": obj{"active_month": app.countInt(`SELECT COUNT(*) FROM users WHERE last_seen > strftime('%s','now') - 30*86400`)}},
		"thumbnail": obj{"url": app.thumbnailURL()},
		"icon":      []any{obj{"src": app.cfg.BaseURL() + "/static/favicon.svg", "size": "64x64"}},
		"languages": []string{"en"}, "configuration": app.apiConfiguration(),
		"registrations": obj{"enabled": app.registrationMode() != "closed", "approval_required": app.registrationMode() == "approval", "message": nil, "url": nil},
		"api_versions":  obj{"mastodon": 7},
		"contact":       obj{"email": "", "account": contact},
		"rules":         app.apiRuleList(),
	}
}
