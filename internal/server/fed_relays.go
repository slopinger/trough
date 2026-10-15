package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"git.freifick.net/slop/trough/internal/ap"
	"git.freifick.net/slop/trough/internal/ulid"
)

// ---- relays ----

// Relays (Mastodon-style pub-relay, LitePub/Pleroma relays, Aode, ...)
// spread public posts between servers. A small instance subscribes to fill
// its federated timeline. Relayed content is never trusted as-is: every post
// is fetched from its origin.

type Relay struct {
	ID, Inbox, ActorURI, FollowURI, Style string
	Accepted                              bool
	CreatedAt                             time.Time
}

func (app *App) relays() []Relay {
	rows, err := app.db.Query(`SELECT id, inbox, actor_uri, follow_uri, style, accepted, created_at FROM relays ORDER BY created_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Relay
	for rows.Next() {
		var r Relay
		var created int64
		rows.Scan(&r.ID, &r.Inbox, &r.ActorURI, &r.FollowURI, &r.Style, &r.Accepted, &created)
		r.CreatedAt = time.Unix(created, 0)
		out = append(out, r)
	}
	return out
}

func (app *App) AddRelay(ctx context.Context, raw string) error {
	u, err := app.checkRemoteURL(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("enter the relay's inbox or actor URL (https://...)")
	}
	inst := app.instanceActor()
	id := ulid.New()
	followURI := inst.URI + "#relays/" + id
	var follow obj
	r := Relay{ID: id, FollowURI: followURI}
	if strings.HasSuffix(u.Path, "/inbox") {
		// Mastodon-style relay: follow the public collection
		r.Inbox, r.Style = u.String(), "mastodon"
		follow = withContext(obj{"id": followURI, "type": "Follow", "actor": inst.URI, "object": ap.Public})
	} else {
		// LitePub-style relay: follow the relay actor
		m, err := app.fetchAP(ctx, u.String())
		if err != nil {
			return errors.New("could not fetch the relay actor: " + err.Error())
		}
		acc, err := app.upsertActor(m)
		if err != nil {
			return err
		}
		r.Inbox, r.ActorURI, r.Style = acc.DeliveryInbox(), acc.URI, "litepub"
		follow = withContext(obj{"id": followURI, "type": "Follow", "actor": inst.URI, "object": acc.URI})
	}
	if _, err := app.db.Exec(`INSERT INTO relays (id, inbox, actor_uri, follow_uri, style, created_at) VALUES (?,?,?,?,?,?)`,
		r.ID, r.Inbox, r.ActorURI, r.FollowURI, r.Style, time.Now().Unix()); err != nil {
		return errors.New("that relay is already added")
	}
	app.enqueue(inst, []string{r.Inbox}, follow)
	return nil
}

func (app *App) RemoveRelay(id string) error {
	var r Relay
	if app.db.QueryRow(`SELECT inbox, actor_uri, follow_uri, style FROM relays WHERE id=?`, id).Scan(&r.Inbox, &r.ActorURI, &r.FollowURI, &r.Style) != nil {
		return errNotFound
	}
	inst := app.instanceActor()
	object := any(ap.Public)
	if r.Style == "litepub" {
		object = r.ActorURI
	}
	app.enqueue(inst, []string{r.Inbox}, undoActivity(inst, obj{"id": r.FollowURI, "type": "Follow", "actor": inst.URI, "object": object}))
	_, err := app.db.Exec(`DELETE FROM relays WHERE id=?`, id)
	return err
}

// relayFor reports whether an actor is a relay we subscribed to.
func (app *App) isRelay(actor *Account) bool {
	if actor.IsLocal() {
		return false
	}
	var x int
	return app.db.QueryRow(`SELECT 1 FROM relays WHERE actor_uri=? OR (style='mastodon' AND actor_uri='' AND inbox LIKE ?)`,
		actor.URI, "%://"+ap.Host(actor.URI)+"/%").Scan(&x) == nil
}

// relayAccept marks a relay subscription as accepted.
func (app *App) relayAccept(actor *Account, followURI string, accepted bool) bool {
	var id string
	if app.db.QueryRow(`SELECT id FROM relays WHERE follow_uri=? OR (actor_uri='' AND inbox LIKE ?)`, followURI, "%://"+ap.Host(actor.URI)+"/%").Scan(&id) != nil {
		return false
	}
	if accepted {
		app.db.Exec(`UPDATE relays SET accepted=1, actor_uri=? WHERE id=?`, actor.URI, id)
	} else {
		app.db.Exec(`DELETE FROM relays WHERE id=?`, id)
	}
	return true
}

func (app *App) relayInboxes() []string {
	rows, err := app.db.Query(`SELECT DISTINCT inbox FROM relays WHERE accepted=1`)
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

// inRelayed handles activities a relay passes on: Announces of posts
// (LitePub) or forwarded Creates/Updates/Deletes (Mastodon). The content is
// always fetched from its origin server.
func (app *App) inRelayed(ctx context.Context, act obj) error {
	o := act["object"]
	if inner, ok := o.(obj); ok && wrappedActivityTypes[ap.Type(inner)] {
		act, o = inner, inner["object"]
	}
	iri := ap.ID(o)
	if iri == "" || app.isLocalIRI(iri) {
		return nil
	}
	switch ap.Type(act) {
	case "Announce", "Create":
		if _, err := app.StatusByURI(iri); err == nil {
			return nil
		}
		st, err := app.resolveStatus(ctx, iri)
		if err != nil {
			return err
		}
		if st.Visibility != "public" {
			app.purgeStatus(st.ID) // relays are for public posts only
		}
		return nil
	case "Update":
		if st, err := app.StatusByURI(iri); err == nil {
			return app.refreshStatus(ctx, st)
		}
	case "Delete":
		if st, err := app.StatusByURI(iri); err == nil {
			if _, err := app.fetchAP(ctx, iri); errors.Is(err, errGone) || errors.Is(err, errNotFound) {
				return app.purgeStatus(st.ID)
			}
		}
	}
	return nil
}

func (app *App) handleAdminRelay(w http.ResponseWriter, r *http.Request, p *Page) {
	if id := r.FormValue("remove"); id != "" {
		app.RemoveRelay(id)
		setFlash(w, "Unsubscribed from the relay.")
	} else if err := app.AddRelay(r.Context(), r.FormValue("url")); err != nil {
		setFlash(w, err.Error())
	} else {
		setFlash(w, "Subscription request sent to the relay.")
	}
	http.Redirect(w, r, "/admin#relays", http.StatusSeeOther)
}
