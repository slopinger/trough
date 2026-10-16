package server

import (
	"mime"
	"path"
	"strings"

	"git.freifick.net/slop/trough/internal/ap"
	"git.freifick.net/slop/trough/internal/ulid"
)

// ActivityPub representations of local objects.

// actorEndpoints also tells ActivityPub C2S clients where to get a token.
func (app *App) actorEndpoints(acc *Account) obj {
	e := obj{"sharedInbox": app.cfg.BaseURL() + "/inbox"}
	if acc.ActorType != "Application" {
		e["oauthAuthorizationEndpoint"] = app.cfg.BaseURL() + "/oauth/authorize"
		e["oauthTokenEndpoint"] = app.cfg.BaseURL() + "/oauth/token"
		e["uploadMedia"] = acc.URI + "/uploadMedia"
	}
	return e
}

func (app *App) actorJSON(acc *Account) obj {
	if acc.Suspended && acc.IsLocal() {
		return app.suspendedActorJSON(acc)
	}
	m := obj{
		"@context":                  ap.Context,
		"id":                        acc.URI,
		"type":                      acc.ActorType,
		"preferredUsername":         acc.Username,
		"name":                      acc.DisplayName,
		"summary":                   acc.Note,
		"url":                       acc.URL,
		"inbox":                     acc.Inbox,
		"outbox":                    acc.Outbox,
		"endpoints":                 app.actorEndpoints(acc),
		"manuallyApprovesFollowers": acc.Locked,
		"discoverable":              acc.Discoverable,
		"indexable":                 false,
		"published":                 ap.Time(acc.CreatedAt),
		"publicKey": obj{
			"id":           acc.PublicKeyID,
			"owner":        acc.URI,
			"publicKeyPem": acc.PublicKey,
		},
		"tag": append([]any{}, app.emojiTags(acc.Emojis)...),
	}
	if acc.FollowersURI != "" {
		m["followers"] = acc.FollowersURI
		m["following"] = acc.FollowingURI
		m["featured"] = acc.URI + "/collections/featured"
		m["featuredTags"] = acc.URI + "/collections/tags"
	}
	if len(acc.AttributionDomains) > 0 {
		m["attributionDomains"] = acc.AttributionDomains
	}
	if len(acc.AlsoKnownAs) > 0 {
		m["alsoKnownAs"] = acc.AlsoKnownAs
	}
	if acc.MovedTo != "" {
		m["movedTo"] = acc.MovedTo
	}
	if acc.Avatar != "" {
		m["icon"] = imageObj(acc.Avatar)
	}
	if acc.Header != "" {
		m["image"] = imageObj(acc.Header)
	}
	var att []any
	for _, f := range acc.Fields {
		att = append(att, obj{"type": "PropertyValue", "name": f.Name, "value": f.Value})
	}
	if att != nil {
		m["attachment"] = att
	}
	return m
}

// suspendedActorJSON is what a suspended local account shows other servers
// (the way Mastodon does): the key and inboxes, but no profile.
func (app *App) suspendedActorJSON(acc *Account) obj {
	m := obj{
		"@context":          ap.Context,
		"id":                acc.URI,
		"type":              acc.ActorType,
		"preferredUsername": acc.Username,
		"name":              "",
		"summary":           "",
		"url":               acc.URL,
		"inbox":             acc.Inbox,
		"outbox":            acc.Outbox,
		"endpoints":         app.actorEndpoints(acc),
		"published":         ap.Time(acc.CreatedAt),
		"suspended":         true,
		"publicKey":         obj{"id": acc.PublicKeyID, "owner": acc.URI, "publicKeyPem": acc.PublicKey},
		"tag":               []any{},
		"attachment":        []any{},
	}
	if acc.FollowersURI != "" {
		m["followers"] = acc.FollowersURI
		m["following"] = acc.FollowingURI
	}
	return m
}

func imageObj(u string) obj {
	o := obj{"type": "Image", "url": u}
	if t := mime.TypeByExtension(path.Ext(u)); t != "" {
		o["mediaType"] = t
	}
	return o
}

// addressing returns to/cc for a local status.
func (app *App) addressing(st *Status) (to, cc []any) {
	followers := st.Account.FollowersURI
	var mentions []any
	for _, m := range st.Mentions {
		mentions = append(mentions, m.URI)
	}
	if st.InReplyToAccount != nil && st.InReplyToAccount.ID != st.AccountID {
		found := false
		for _, m := range st.Mentions {
			if m.ID == st.InReplyToAccount.ID {
				found = true
			}
		}
		if !found && st.Visibility != "private" {
			mentions = append(mentions, st.InReplyToAccount.URI)
		}
	}
	switch st.Visibility {
	case "public":
		to = []any{ap.Public}
		cc = append([]any{followers}, mentions...)
	case "unlisted":
		to = []any{followers}
		cc = append([]any{ap.Public}, mentions...)
	case "private":
		to = []any{followers}
		cc = mentions
	default:
		to = mentions
	}
	if to == nil {
		to = []any{}
	}
	if cc == nil {
		cc = []any{}
	}
	return
}

func (app *App) noteJSON(st *Status) obj {
	to, cc := app.addressing(st)
	tags := []any{}
	for _, m := range st.Mentions {
		tags = append(tags, obj{"type": "Mention", "href": m.URI, "name": "@" + m.Username + "@" + app.accountHost(m)})
	}
	for _, t := range st.Tags {
		tags = append(tags, obj{"type": "Hashtag", "href": app.cfg.BaseURL() + "/tags/" + t, "name": "#" + t})
	}
	tags = append(tags, app.emojiTags(st.Emojis)...)
	atts := []any{}
	for _, a := range st.Attachments {
		o := obj{"type": "Document", "mediaType": a.MediaType, "url": a.URL, "name": nil}
		if a.Description != "" {
			o["name"] = a.Description
		}
		if a.Width > 0 {
			o["width"], o["height"] = a.Width, a.Height
		}
		if a.PreviewURL != "" {
			o["icon"] = imageObj(a.PreviewURL)
		}
		atts = append(atts, o)
	}
	m := obj{
		"id":           st.URI,
		"type":         "Note",
		"url":          st.URL,
		"attributedTo": st.Account.URI,
		"published":    ap.Time(st.CreatedAt),
		"to":           to,
		"cc":           cc,
		"content":      st.Content,
		"summary":      nil,
		"sensitive":    st.Sensitive,
		"inReplyTo":    nil,
		"tag":          tags,
		"attachment":   atts,
		// shaped like Mastodon's and GoToSocial's: an embedded first page
		// that links the real one (GoToSocial needs the embedded page)
		"replies": obj{
			"id":   st.URI + "/replies",
			"type": "Collection",
			"first": obj{"type": "CollectionPage", "partOf": st.URI + "/replies",
				"next": st.URI + "/replies?page=true", "items": []any{}},
		},
	}
	if st.Visibility == "private" || st.Visibility == "direct" {
		delete(m, "replies") // the collection is only served for public posts
	}
	if st.Language != "" {
		m["contentMap"] = obj{st.Language: st.Content}
	}
	if st.Spoiler != "" {
		m["summary"] = st.Spoiler
	}
	if st.InReplyToURI != "" {
		m["inReplyTo"] = st.InReplyToURI
	}
	if !st.EditedAt.IsZero() {
		m["updated"] = ap.Time(st.EditedAt)
	}
	app.questionFields(st, m)
	app.quoteFields(st, m)
	m["interactionPolicy"] = app.interactionPolicyJSON(st)
	return m
}

func (app *App) accountHost(a *Account) string {
	if a.IsLocal() {
		return app.cfg.Host
	}
	return a.Domain
}

func withContext(m obj) obj {
	m["@context"] = ap.Context
	return m
}

func (app *App) createActivity(st *Status) obj {
	note := app.noteJSON(st)
	return withContext(obj{
		"id":        st.URI + "/activity",
		"type":      "Create",
		"actor":     st.Account.URI,
		"published": note["published"],
		"to":        note["to"],
		"cc":        note["cc"],
		"object":    note,
	})
}

func (app *App) updateNoteActivity(st *Status) obj {
	note := app.noteJSON(st)
	return withContext(obj{
		"id":     st.URI + "#updates/" + ulid.New(),
		"type":   "Update",
		"actor":  st.Account.URI,
		"to":     note["to"],
		"cc":     note["cc"],
		"object": note,
	})
}

func (app *App) announceActivity(boost *Status) obj {
	orig := boost.BoostOf
	to := []any{ap.Public}
	cc := []any{orig.Account.URI, boost.Account.FollowersURI}
	if orig.Visibility == "unlisted" {
		to = []any{boost.Account.FollowersURI}
		cc = []any{ap.Public, orig.Account.URI}
	}
	return withContext(obj{
		"id":        boost.URI,
		"type":      "Announce",
		"actor":     boost.Account.URI,
		"published": ap.Time(boost.CreatedAt),
		"to":        to,
		"cc":        cc,
		"object":    orig.URI,
	})
}

func undoActivity(actor *Account, inner obj) obj {
	delete(inner, "@context")
	return withContext(obj{
		"id":     actor.URI + "#undo/" + ulid.New(),
		"type":   "Undo",
		"actor":  actor.URI,
		"object": inner,
	})
}

func likeActivity(actor *Account, likeID string, st *Status) obj {
	return withContext(obj{
		"id":     actor.URI + "#likes/" + likeID,
		"type":   "Like",
		"actor":  actor.URI,
		"object": st.URI,
	})
}

func followActivity(followURI string, actor, target *Account) obj {
	return withContext(obj{
		"id":     followURI,
		"type":   "Follow",
		"actor":  actor.URI,
		"object": target.URI,
	})
}

func acceptRejectActivity(kind string, me, follower *Account, followURI string) obj {
	return withContext(obj{
		"id":    me.URI + "#" + strings.ToLower(kind) + "s/" + ulid.New(),
		"type":  kind,
		"actor": me.URI,
		"object": obj{
			"id":     followURI,
			"type":   "Follow",
			"actor":  follower.URI,
			"object": me.URI,
		},
	})
}

func blockActivity(blockURI string, actor, target *Account) obj {
	return withContext(obj{
		"id":     blockURI,
		"type":   "Block",
		"actor":  actor.URI,
		"object": target.URI,
	})
}

func (app *App) deleteActivity(st *Status) obj {
	to, cc := app.addressing(st)
	return withContext(obj{
		"id":     st.URI + "#delete",
		"type":   "Delete",
		"actor":  st.Account.URI,
		"to":     to,
		"cc":     cc,
		"object": obj{"id": st.URI, "type": "Tombstone"},
	})
}

func (app *App) updateActorActivity(acc *Account) obj {
	actor := app.actorJSON(acc)
	delete(actor, "@context")
	return withContext(obj{
		"id":     acc.URI + "#updates/" + ulid.New(),
		"type":   "Update",
		"actor":  acc.URI,
		"to":     []any{ap.Public},
		"cc":     []any{acc.FollowersURI},
		"object": actor,
	})
}

// ---- recipients ----

func (app *App) followerInboxes(accountID string) []string {
	rows, err := app.db.Query(`SELECT DISTINCT CASE WHEN a.shared_inbox != '' THEN a.shared_inbox ELSE a.inbox END
		FROM follows f JOIN accounts a ON a.id=f.account_id
		WHERE f.target_id=? AND f.accepted=1 AND a.domain != '' AND a.suspended=0`, accountID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// statusInboxes are the remote inboxes a local status (or its deletion) goes to.
func (app *App) statusInboxes(st *Status) []string {
	var out []string
	if st.Visibility != "direct" {
		out = app.followerInboxes(st.AccountID)
	}
	for _, m := range st.Mentions {
		if !m.IsLocal() {
			out = append(out, m.DeliveryInbox())
		}
	}
	if st.InReplyToAccount != nil && !st.InReplyToAccount.IsLocal() && st.Visibility != "private" {
		out = append(out, st.InReplyToAccount.DeliveryInbox())
	}
	if st.Visibility == "public" && !st.IsBoost() && st.Local {
		out = append(out, app.relayInboxes()...)
	}
	return out
}
