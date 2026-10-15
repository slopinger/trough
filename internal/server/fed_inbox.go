package server

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"git.freifick.net/slop/trough/internal/ap"
	"git.freifick.net/slop/trough/internal/ulid"
)

type inboxJob struct {
	signer   *Account
	activity obj
}

// handleInbox serves POST /inbox and /users/{name}/inbox.
func (app *App) handleInbox(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONSize+1))
	if err != nil || len(body) > maxJSONSize {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var act obj
	if err := jsonUnmarshal(body, &act); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	actorIRI := ap.ID(act["actor"])
	if actorIRI == "" {
		http.Error(w, "activity without actor", http.StatusBadRequest)
		return
	}
	if app.domainBlocked(ap.Host(actorIRI)) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	// Deletes of accounts we never heard of are very common (servers
	// broadcast them everywhere); don't bother fetching a key that is gone.
	if ap.Type(act) == "Delete" && ap.ID(act["object"]) == actorIRI {
		if _, err := app.AccountByURI(actorIRI); err != nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}
	signer, err := app.verifyRequest(r, body)
	if err != nil {
		if errors.Is(err, errBlocked) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		log.Printf("inbox: rejecting %s %s from %s: %v", ap.Type(act), ap.Str(act, "id"), actorIRI, err)
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}
	if signer.URI != ap.StripFragment(actorIRI) && !app.isRelay(signer) {
		log.Printf("inbox: signer %s does not match actor %s", signer.URI, actorIRI)
		http.Error(w, "signer does not match actor", http.StatusUnauthorized)
		return
	}
	if signer.Suspended {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	select {
	case app.inboxQueue <- inboxJob{signer, act}:
		w.WriteHeader(http.StatusAccepted)
	default:
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}
}

func (app *App) inboxWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-app.inboxQueue:
			jctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			if err := app.processActivity(jctx, job.signer, job.activity); err != nil {
				log.Printf("inbox: %s %s from %s: %v", ap.Type(job.activity), ap.Str(job.activity, "id"), job.signer.URI, err)
			}
			cancel()
		}
	}
}

func (app *App) isLocalIRI(iri string) bool {
	return strings.HasPrefix(iri, app.cfg.BaseURL()+"/")
}

func (app *App) processActivity(ctx context.Context, actor *Account, act obj) error {
	if app.isRelay(actor) {
		switch ap.Type(act) {
		case "Announce", "Create", "Update", "Delete":
			return app.inRelayed(ctx, act)
		}
	}
	switch ap.Type(act) {
	case "Follow":
		return app.inFollow(actor, act)
	case "Accept", "Reject":
		if app.quoteAnswer(actor, act) {
			return nil
		}
		return app.inAcceptReject(ctx, actor, act)
	case "QuoteRequest":
		return app.inQuoteRequest(ctx, actor, act)
	case "Undo":
		return app.inUndo(actor, act)
	case "Create":
		return app.inCreate(ctx, actor, act)
	case "Update":
		return app.inUpdate(ctx, actor, act)
	case "Delete":
		return app.inDelete(actor, act)
	case "Like":
		return app.inLike(actor, act)
	case "Announce":
		return app.inAnnounce(ctx, actor, act)
	case "Block":
		return app.inBlock(actor, act)
	case "Add", "Remove":
		return app.inAddRemove(ctx, actor, act)
	case "Flag":
		return app.inFlag(actor, act)
	case "EmojiReact":
		return app.inEmojiReact(actor, act)
	case "Move":
		return app.inMove(ctx, actor, act)
	}
	return nil
}

func (app *App) localTarget(iri string) (*Account, error) {
	if !app.isLocalIRI(iri) {
		return nil, errNotFound
	}
	acc, err := app.AccountByURI(iri)
	if err != nil || acc.ActorType == "Application" {
		return nil, errNotFound
	}
	return acc, nil
}

func (app *App) inFollow(actor *Account, act obj) error {
	target, err := app.localTarget(ap.ID(act["object"]))
	if err != nil {
		return err
	}
	followURI := ap.Str(act, "id")
	if followURI == "" {
		return errors.New("follow without id")
	}
	if app.isBlockedBy(target.ID, actor.ID) {
		app.enqueue(target, []string{actor.Inbox}, acceptRejectActivity("Reject", target, actor, followURI))
		return nil
	}
	var accepted bool
	err = app.db.QueryRow(`SELECT accepted FROM follows WHERE account_id=? AND target_id=?`, actor.ID, target.ID).Scan(&accepted)
	if err == nil {
		// already known: update the uri (Mastodon re-follows with a new id) and re-accept
		app.db.Exec(`UPDATE follows SET uri=? WHERE account_id=? AND target_id=?`, followURI, actor.ID, target.ID)
		if accepted {
			app.enqueue(target, []string{actor.Inbox}, acceptRejectActivity("Accept", target, actor, followURI))
		}
		return nil
	}
	accepted = !target.Locked
	if _, err := app.db.Exec(`INSERT INTO follows (id,uri,account_id,target_id,accepted,created_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(uri) DO NOTHING`, ulid.New(), followURI, actor.ID, target.ID, accepted, time.Now().Unix()); err != nil {
		return err
	}
	if accepted {
		app.notify(target.ID, "follow", actor.ID, "")
		app.enqueue(target, []string{actor.Inbox}, acceptRejectActivity("Accept", target, actor, followURI))
	} else {
		app.notify(target.ID, "follow_request", actor.ID, "")
	}
	return nil
}

func (app *App) inAcceptReject(ctx context.Context, actor *Account, act obj) error {
	o := act["object"]
	followURI := ap.ID(o)
	var followerID string
	err := app.db.QueryRow(`SELECT account_id FROM follows WHERE uri=? AND target_id=?`, followURI, actor.ID).Scan(&followerID)
	if err != nil {
		// some servers only embed actor/object of our Follow
		if m, ok := o.(obj); ok && ap.Type(m) == "Follow" {
			if me, err := app.localTarget(ap.ID(m["actor"])); err == nil {
				followerID = me.ID
			}
		}
		if followerID == "" {
			if app.relayAccept(actor, followURI, ap.Type(act) == "Accept") {
				return nil
			}
			return errors.New("unknown follow")
		}
	}
	if ap.Type(act) == "Accept" {
		app.db.Exec(`UPDATE follows SET accepted=1 WHERE account_id=? AND target_id=?`, followerID, actor.ID)
		go app.backfillOutbox(context.Background(), actor)
	} else {
		app.db.Exec(`DELETE FROM follows WHERE account_id=? AND target_id=?`, followerID, actor.ID)
	}
	return nil
}

func (app *App) inUndo(actor *Account, act obj) error {
	o := act["object"]
	innerID := ap.ID(o)
	inner, _ := o.(obj)
	kind := ""
	if inner != nil {
		kind = ap.Type(inner)
		if a := ap.ID(inner["actor"]); a != "" && a != actor.URI {
			return errors.New("undo of someone else's activity")
		}
	}
	if kind == "Like" || kind == "EmojiReact" || kind == "" {
		app.db.Exec(`DELETE FROM reactions WHERE uri=? AND account_id=?`, innerID, actor.ID)
		if kind == "EmojiReact" {
			return nil
		}
	}
	switch kind {
	case "Follow", "":
		var targetID string
		err := app.db.QueryRow(`DELETE FROM follows WHERE uri=? AND account_id=? RETURNING target_id`, innerID, actor.ID).Scan(&targetID)
		if errors.Is(err, sql.ErrNoRows) && inner != nil {
			if target, targetErr := app.localTarget(ap.ID(inner["object"])); targetErr == nil {
				err = app.db.QueryRow(`DELETE FROM follows WHERE account_id=? AND target_id=? RETURNING target_id`, actor.ID, target.ID).Scan(&targetID)
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if _, err := app.db.Exec(`DELETE FROM notifications WHERE account_id=? AND from_account_id=? AND type IN ('follow','follow_request')`, targetID, actor.ID); err != nil {
				return err
			}
		}
		if kind == "Follow" {
			return nil
		}
		fallthrough
	case "Like":
		var statusID string
		err := app.db.QueryRow(`DELETE FROM likes WHERE uri=? AND account_id=? RETURNING status_id`, innerID, actor.ID).Scan(&statusID)
		if errors.Is(err, sql.ErrNoRows) && inner != nil {
			if st, statusErr := app.StatusByURI(ap.ID(inner["object"])); statusErr == nil {
				err = app.db.QueryRow(`DELETE FROM likes WHERE account_id=? AND status_id=? RETURNING status_id`, actor.ID, st.ID).Scan(&statusID)
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if _, err := app.db.Exec(`DELETE FROM notifications WHERE type='favourite' AND from_account_id=? AND status_id=?`, actor.ID, statusID); err != nil {
				return err
			}
		}
		if kind == "Like" {
			return nil
		}
		fallthrough
	case "Announce":
		if b, err := app.statusWhere(`s.uri=? AND s.account_id=? AND s.boost_of_id != ''`, innerID, actor.ID); err == nil {
			app.db.Exec(`DELETE FROM notifications WHERE type='reblog' AND from_account_id=? AND status_id=?`, actor.ID, b.BoostOfID)
			app.db.Exec(`DELETE FROM statuses WHERE id=?`, b.ID)
		}
		if kind == "Announce" {
			return nil
		}
		fallthrough
	case "Block":
		app.db.Exec(`DELETE FROM blocks WHERE uri=? AND account_id=?`, innerID, actor.ID)
	}
	return nil
}

func (app *App) inBlock(actor *Account, act obj) error {
	target, err := app.localTarget(ap.ID(act["object"]))
	if err != nil {
		return err
	}
	app.db.Exec(`INSERT OR IGNORE INTO blocks (id,uri,account_id,target_id,created_at) VALUES (?,?,?,?,?)`,
		ulid.New(), ap.Str(act, "id"), actor.ID, target.ID, time.Now().Unix())
	app.db.Exec(`DELETE FROM follows WHERE (account_id=? AND target_id=?) OR (account_id=? AND target_id=?)`, actor.ID, target.ID, target.ID, actor.ID)
	return nil
}

func (app *App) inLike(actor *Account, act obj) error {
	st, err := app.StatusByURI(ap.ID(act["object"]))
	if err != nil {
		return nil // not something we know about
	}
	uri := ap.Str(act, "id")
	if uri == "" {
		uri = actor.URI + "#likes/" + st.ID
	}
	res, err := app.db.Exec(`INSERT OR IGNORE INTO likes (id,uri,account_id,status_id,created_at) VALUES (?,?,?,?,?)`,
		ulid.New(), uri, actor.ID, st.ID, time.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 && st.Local {
		app.notify(st.AccountID, "favourite", actor.ID, st.ID)
	}
	// Misskey sends reactions as Likes with the emoji as content
	reaction := ap.Str(act, "_misskey_reaction")
	if reaction == "" {
		reaction = ap.Str(act, "content")
	}
	if reaction != "" {
		app.storeReaction(actor, st, act, reaction)
	}
	return nil
}

func (app *App) inAnnounce(ctx context.Context, actor *Account, act obj) error {
	uri := ap.Str(act, "id")
	if uri == "" {
		return errors.New("announce without id")
	}
	if _, err := app.StatusByURI(uri); err == nil {
		return nil
	}
	if inner, ok := act["object"].(obj); ok && wrappedActivityTypes[ap.Type(inner)] {
		return app.inAnnouncedActivity(ctx, actor, act, inner)
	}
	objIRI := ap.ID(act["object"])
	orig, err := app.StatusByURI(objIRI)
	if err != nil {
		// only pull in boosted posts for accounts somebody here follows
		if !app.hasLocalFollowers(actor.ID) {
			return nil
		}
		orig, err = app.resolveStatus(ctx, objIRI)
		if err != nil {
			return err
		}
	}
	if orig.Visibility != "public" && orig.Visibility != "unlisted" {
		return nil
	}
	published := ap.ParseTime(ap.Str(act, "published"))
	if published.IsZero() || published.After(time.Now()) {
		published = time.Now()
	}
	b := &Status{
		ID: ulid.At(published), URI: uri, URL: uri, AccountID: actor.ID, BoostOfID: orig.ID,
		Visibility: ap.Visibility(act, actor.FollowersURI), CreatedAt: published,
	}
	if b.Visibility != "public" && b.Visibility != "unlisted" {
		b.Visibility = "unlisted"
	}
	if err := app.insertStatus(b); err != nil {
		return err
	}
	app.publishStatus(b.ID, false)
	if orig.Local {
		app.notify(orig.AccountID, "reblog", actor.ID, orig.ID)
	}
	return nil
}

var wrappedActivityTypes = map[string]bool{"Create": true, "Update": true, "Delete": true, "Like": true, "Dislike": true,
	"Undo": true, "Add": true, "Remove": true, "Lock": true, "Block": true, "Flag": true, "Follow": true, "Announce": true}

// inAnnouncedActivity handles groups (Lemmy communities, Friendica forums,
// guppe, ...) relaying activities of their members by wrapping them in an
// Announce. The wrapped activity isn't signed by its author, so everything
// is re-fetched from the origin server instead of trusting the embed.
func (app *App) inAnnouncedActivity(ctx context.Context, group *Account, act, inner obj) error {
	objIRI := ap.ID(inner["object"])
	if objIRI == "" {
		return nil
	}
	switch ap.Type(inner) {
	case "Create", "Update":
		existing, err := app.StatusByURI(objIRI)
		if err == nil {
			if ap.Type(inner) == "Update" {
				return app.refreshStatus(ctx, existing)
			}
		} else {
			if !app.hasLocalFollowers(group.ID) {
				return nil
			}
			if existing, err = app.resolveStatus(ctx, objIRI); err != nil {
				return err
			}
		}
		if ap.Type(inner) != "Create" || existing.Visibility == "private" || existing.Visibility == "direct" {
			return nil
		}
		boostURI := ap.Str(act, "id")
		if _, err := app.StatusByURI(boostURI); err == nil || boostURI == "" {
			return nil
		}
		b := &Status{ID: ulid.At(existing.CreatedAt), URI: boostURI, URL: boostURI, AccountID: group.ID, BoostOfID: existing.ID,
			Visibility: "public", CreatedAt: existing.CreatedAt}
		if !existing.Local && existing.InReplyToURI != "" {
			return nil // comments show up in threads, not as boosts in timelines
		}
		if err := app.insertStatus(b); err != nil {
			return err
		}
		app.publishStatus(b.ID, false)
		return nil
	case "Delete":
		st, err := app.StatusByURI(objIRI)
		if err != nil {
			return nil
		}
		// only believe the deletion if the origin no longer serves the post
		m, err := app.fetchAP(ctx, objIRI)
		if errors.Is(err, errGone) || errors.Is(err, errNotFound) || (err == nil && ap.Type(m) == "Tombstone") {
			return app.purgeStatus(st.ID)
		}
		return nil
	}
	return nil
}

// refreshStatus re-fetches a remote post from its origin and applies edits.
func (app *App) refreshStatus(ctx context.Context, st *Status) error {
	if st.Local {
		return nil
	}
	m, err := app.fetchAP(ctx, st.URI)
	if err != nil {
		return err
	}
	acc, err := app.AccountByID(st.AccountID)
	if err != nil {
		return err
	}
	if _, listed := attributedTo(m, acc.URI); !listed || ap.Str(m, "id") != st.URI {
		return errors.New("refetched post changed author or id")
	}
	app.applyNote(st, m)
	if err := app.updateStatusContent(st); err != nil {
		return err
	}
	app.replaceNoteExtras(ctx, st, m)
	return nil
}

func (app *App) hasLocalFollowers(accountID string) bool {
	var x int
	return app.db.QueryRow(`SELECT 1 FROM follows f JOIN accounts a ON a.id=f.account_id WHERE f.target_id=? AND f.accepted=1 AND a.domain='' LIMIT 1`, accountID).Scan(&x) == nil
}

var noteTypes = map[string]bool{"Note": true, "Article": true, "Question": true, "Page": true, "Video": true, "Event": true, "Audio": true, "Image": true}

func (app *App) inCreate(ctx context.Context, actor *Account, act obj) error {
	o := act["object"]
	note, ok := o.(obj)
	if !ok {
		// only a reference; fetch it
		m, err := app.fetchAP(ctx, ap.ID(o))
		if err != nil {
			return err
		}
		note = m
	}
	if !noteTypes[ap.Type(note)] {
		return nil
	}
	sender, author := actor, actor
	if a, listed := attributedTo(note, actor.URI); !listed || !ap.SameHost(ap.Str(note, "id"), actor.URI) {
		// forwarded or forged: trust only what the origin server says
		m, err := app.fetchAP(ctx, ap.Str(note, "id"))
		if err != nil {
			return err
		}
		note = m
		a, _ = attributedTo(note, "")
		author, err = app.getAccount(ctx, a)
		if err != nil {
			return err
		}
		if !ap.SameHost(ap.Str(note, "id"), author.URI) {
			return errors.New("note and author on different hosts")
		}
	}
	if app.inPollVote(author, note) {
		return nil
	}
	if !app.relevant(author, note) && !app.hasLocalFollowers(sender.ID) {
		return nil
	}
	_, err := app.storeNote(ctx, author, note, 0)
	return err
}

// attributedTo picks the author IRI of an object. attributedTo may be an
// array (PeerTube lists the account and the channel); the sender wins if it
// is listed, otherwise the first Person.
func attributedTo(note obj, sender string) (string, bool) {
	ids := ap.IDs(note["attributedTo"])
	for _, id := range ids {
		if id == sender && sender != "" {
			return id, true
		}
	}
	for _, o := range ap.Objects(note["attributedTo"]) {
		if ap.Type(o) == "Person" && ap.Str(o, "id") != "" {
			return ap.Str(o, "id"), false
		}
	}
	if len(ids) > 0 {
		return ids[0], false
	}
	return "", false
}

// relevant decides whether we want an incoming post: it is by someone a
// local user follows, addresses a local user, or replies to a local post.
func (app *App) relevant(author *Account, note obj) bool {
	if app.hasLocalFollowers(author.ID) {
		return true
	}
	for _, r := range ap.Recipients(note) {
		if app.isLocalIRI(r) {
			return true
		}
	}
	for _, t := range ap.Objects(note["tag"]) {
		if ap.Type(t) == "Mention" && app.isLocalIRI(ap.Str(t, "href")) {
			return true
		}
	}
	if p := ap.ID(note["inReplyTo"]); p != "" {
		if _, err := app.StatusByURI(p); err == nil {
			return true
		}
	}
	return false
}

func (app *App) inUpdate(ctx context.Context, actor *Account, act obj) error {
	o, _ := act["object"].(obj)
	if o == nil {
		return nil
	}
	if actorTypes[ap.Type(o)] {
		if ap.Str(o, "id") != actor.URI {
			return errors.New("update of another actor")
		}
		_, err := app.upsertActor(o)
		return err
	}
	if noteTypes[ap.Type(o)] {
		st, err := app.StatusByURI(ap.Str(o, "id"))
		if err != nil {
			return nil
		}
		if _, listed := attributedTo(o, actor.URI); st.AccountID != actor.ID || !listed {
			return errors.New("update of someone else's post")
		}
		before := [3]string{st.Content, st.Spoiler, pollText(st.Poll)}
		app.applyNote(st, o)
		st.QuotePolicy = remoteQuotePolicy(o, actor)
		if state := app.localQuoteState(st, o); state != "pending" || st.QuoteState != "revoked" {
			st.QuoteState = state
		}
		if err := app.updateStatusContent(st); err != nil {
			return err
		}
		app.replaceNoteExtras(ctx, st, o)
		app.publishStatus(st.ID, true)
		if before != [3]string{st.Content, st.Spoiler, pollText(st.Poll)} {
			app.notifyEdited(st) // not for vote counts or quote stamps
		}
	}
	return nil
}

func (app *App) inDelete(actor *Account, act obj) error {
	iri := ap.ID(act["object"])
	if iri == actor.URI {
		// the account is gone; remove what we have of it
		log.Printf("inbox: account %s deleted", actor.URI)
		app.db.Exec(`DELETE FROM accounts WHERE id=?`, actor.ID)
		return nil
	}
	st, err := app.StatusByURI(iri)
	if err != nil {
		app.quoteRevoked(actor, iri)
		return nil
	}
	if st.AccountID != actor.ID {
		return errors.New("delete of someone else's post")
	}
	return app.purgeStatus(st.ID)
}

// pollText is what an edit of a poll can change: its options.
func pollText(p *Poll) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	for _, o := range p.Options {
		b.WriteString(o.Name + "\n")
	}
	return b.String()
}
