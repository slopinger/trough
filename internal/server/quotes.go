package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Quote posts with consent (FEP-044f, as Mastodon 4.5+ and GoToSocial do
// it). Posts say who may quote them (interactionPolicy.canQuote). To quote
// someone else's post, the quoting server sends a QuoteRequest to the
// quoted author's server, which answers Accept with a QuoteAuthorization
// stamp (or Reject). The quoting post then carries the stamp in
// quoteAuthorization, and the quoted author can revoke it by deleting the
// stamp. Quotes from software without this (Misskey, Akkoma, older
// servers) keep working as before.

var quotePolicies = map[string]bool{"public": true, "followers": true, "nobody": true}

// quotePolicy says who may quote st: public, followers or nobody, and ""
// when a remote server didn't say. Only public and unlisted posts can be
// quoted by others.
func quotePolicy(st *Status) string {
	if st.Local && st.Visibility != "public" && st.Visibility != "unlisted" {
		return "nobody"
	}
	if st.Local && st.QuotePolicy == "" {
		return "public"
	}
	return st.QuotePolicy
}

// defaultQuotePolicy is who may quote a user's new posts unless they say
// otherwise when posting.
func (app *App) defaultQuotePolicy(accountID string) string {
	policy := "public"
	app.db.QueryRow(`SELECT quote_policy FROM users WHERE account_id=?`, accountID).Scan(&policy)
	return policy
}

func (app *App) SetDefaultQuotePolicy(me *Account, policy string) error {
	if !quotePolicies[policy] {
		return errors.New("who may quote must be public, followers or nobody")
	}
	_, err := app.db.Exec(`UPDATE users SET quote_policy=? WHERE account_id=?`, policy, me.ID)
	return err
}

func (app *App) followsAccepted(followerID, targetID string) bool {
	var x int
	return app.db.QueryRow(`SELECT 1 FROM follows WHERE account_id=? AND target_id=? AND accepted=1`, followerID, targetID).Scan(&x) == nil
}

// quoteApproval says whether who may quote st: automatic (the quote shows
// right away), denied, or unknown (a remote post without a policy; trough
// shows such quotes right away too).
func (app *App) quoteApproval(st *Status, who *Account) string {
	if who == nil || st.IsBoost() || st.Visibility == "direct" {
		return "denied"
	}
	if who.ID == st.AccountID {
		return "automatic"
	}
	if st.Visibility == "private" {
		return "denied"
	}
	switch quotePolicy(st) {
	case "public":
		return "automatic"
	case "followers":
		if app.followsAccepted(who.ID, st.AccountID) {
			return "automatic"
		}
	case "":
		return "unknown"
	}
	return "denied"
}

// interactionPolicyJSON is the canQuote part of a local note.
func (app *App) interactionPolicyJSON(st *Status) obj {
	var who []any
	switch quotePolicy(st) {
	case "public":
		who = []any{asPublic}
	case "followers":
		who = []any{st.Account.FollowersURI}
	default:
		who = []any{st.Account.URI}
	}
	return obj{"canQuote": obj{"automaticApproval": who}}
}

// remoteQuotePolicy reads canQuote of a remote note into public, followers,
// nobody or "" (no policy).
func remoteQuotePolicy(note obj, author *Account) string {
	ip, _ := note["interactionPolicy"].(obj)
	cq, _ := ip["canQuote"].(obj)
	if cq == nil {
		return ""
	}
	auto := idsOf(cq["automaticApproval"])
	for _, id := range append(auto, idsOf(cq["manualApproval"])...) {
		if isPublicIRI(id) {
			return "public"
		}
	}
	for _, id := range auto {
		if id == author.FollowersURI && id != "" {
			return "followers"
		}
	}
	return "nobody"
}

func (app *App) quoteAuthURI(acc *Account, id string) string {
	return app.actorURI(acc.Username) + "/quote_authorizations/" + id
}

// authorizeQuote records that the author of the local post quoted lets
// quoting (by author) quote it, and returns the stamp's IRI.
func (app *App) authorizeQuote(quoted *Status, quotingURI string, author *Account) (string, error) {
	var id string
	err := app.db.QueryRow(`SELECT id FROM quote_authorizations WHERE status_id=? AND quoting_uri=?`, quoted.ID, quotingURI).Scan(&id)
	if err != nil {
		id = newID()
		if _, err := app.db.Exec(`INSERT INTO quote_authorizations (id, status_id, quoting_uri, account_id, created_at) VALUES (?,?,?,?,?)`,
			id, quoted.ID, quotingURI, author.ID, time.Now().Unix()); err != nil {
			return "", err
		}
	}
	acc := quoted.Account
	if acc == nil {
		if acc, err = app.AccountByID(quoted.AccountID); err != nil {
			return "", err
		}
	}
	return app.quoteAuthURI(acc, id), nil
}

// localQuoteState decides how a quote of quotedURI by a remote note is
// shown. Only quotes of our own posts are checked: we hand out their
// stamps. Quotes by software that doesn't know the handshake (no
// FEP-044f "quote" property) are let through, like everything else.
func (app *App) localQuoteState(st *Status, note obj) string {
	if st.QuoteURI == "" {
		return ""
	}
	q, err := app.StatusByURI(st.QuoteURI)
	if err != nil || !q.Local || q.AccountID == st.AccountID {
		return ""
	}
	var x int
	if app.db.QueryRow(`SELECT 1 FROM quote_authorizations WHERE status_id=? AND quoting_uri=?`, q.ID, st.URI).Scan(&x) == nil {
		return ""
	}
	if _, fep := note["quote"]; !fep {
		return ""
	}
	return "pending"
}

func quoteRequestID(st *Status) string { return st.URI + "#quote-request" }

// quoteRequestActivity asks the author of quoted to approve st.
func (app *App) quoteRequestActivity(st *Status, quoted *Status) obj {
	return withContext(obj{
		"id":         quoteRequestID(st),
		"type":       "QuoteRequest",
		"actor":      st.Account.URI,
		"object":     quoted.URI,
		"instrument": app.noteJSON(st),
	})
}

// inQuoteRequest answers a QuoteRequest for one of our posts.
func (app *App) inQuoteRequest(ctx context.Context, actor *Account, act obj) error {
	reqID := str(act, "id")
	if reqID == "" || !sameHost(reqID, actor.URI) {
		return errors.New("quote request from another host")
	}
	quoted, err := app.StatusByURI(idOf(act["object"]))
	if err != nil || !quoted.Local || quoted.IsBoost() {
		return errNotFound
	}
	owner, err := app.AccountByID(quoted.AccountID)
	if err != nil {
		return err
	}
	quoted.Account = owner
	instURI := idOf(act["instrument"])
	answer := obj{"id": reqID, "type": "QuoteRequest", "actor": actor.URI, "object": quoted.URI, "instrument": instURI}
	if app.quoteApproval(quoted, actor) != "automatic" || app.isBlockedBy(owner.ID, actor.ID) || instURI == "" || !sameHost(instURI, actor.URI) {
		app.enqueue(owner, []string{actor.DeliveryInbox()}, withContext(obj{
			"id": owner.URI + "#rejects/quote_requests/" + newID(), "type": "Reject", "actor": owner.URI, "object": answer,
		}))
		return nil
	}
	quoting, err := app.StatusByURI(instURI)
	if err != nil {
		if inst, ok := act["instrument"].(obj); ok && str(inst, "id") == instURI && attributedToIncludes(inst, actor.URI) {
			quoting, err = app.storeNote(ctx, actor, inst, 0)
		} else {
			quoting, err = app.resolveStatus(ctx, instURI)
		}
		if err != nil {
			return err
		}
	}
	if quoting.AccountID != actor.ID || quoting.QuoteURI != quoted.URI {
		return errors.New("quote request does not match the quoting post")
	}
	stamp, err := app.authorizeQuote(quoted, quoting.URI, actor)
	if err != nil {
		return err
	}
	if quoting.QuoteState != "" || quoting.QuoteAuth != stamp {
		app.db.Exec(`UPDATE statuses SET quote_state='', quote_auth=? WHERE id=?`, stamp, quoting.ID)
		app.publishStatus(quoting.ID, true)
	}
	app.notify(owner.ID, "quote", actor.ID, quoting.ID)
	app.enqueue(owner, []string{actor.DeliveryInbox()}, withContext(obj{
		"id": owner.URI + "#accepts/quote_requests/" + newID(), "type": "Accept", "actor": owner.URI,
		"object": answer, "result": stamp,
	}))
	return nil
}

// quoteAnswer handles Accept/Reject of a QuoteRequest we sent. It reports
// whether the activity was one.
func (app *App) quoteAnswer(actor *Account, act obj) bool {
	reqID := idOf(act["object"])
	uri, ok := strings.CutSuffix(reqID, "#quote-request")
	if !ok || !app.isLocalIRI(uri) {
		return false
	}
	st, err := app.StatusByURI(uri)
	if err != nil || !st.Local || st.QuoteURI == "" {
		return true
	}
	quoted, err := app.StatusByURI(st.QuoteURI)
	if err != nil || quoted.AccountID != actor.ID {
		return true
	}
	if typeOf(act) == "Reject" {
		app.db.Exec(`UPDATE statuses SET quote_state='rejected', quote_auth='' WHERE id=?`, st.ID)
		app.publishStatus(st.ID, true)
		return true
	}
	stamp := idOf(act["result"])
	if stamp == "" || !sameHost(stamp, actor.URI) {
		return true
	}
	app.db.Exec(`UPDATE statuses SET quote_state='', quote_auth=? WHERE id=?`, stamp, st.ID)
	app.publishStatus(st.ID, true)
	if me, err := app.AccountByID(st.AccountID); err == nil {
		if full, err := app.loadStatus(me, st.ID); err == nil {
			// followers' servers learn about the approval from the stamp in the note
			app.enqueue(me, append(app.statusInboxes(full), actor.DeliveryInbox()), app.updateNoteActivity(full))
		}
	}
	return true
}

// quoteRevoked handles the Delete of a stamp by the author of a quoted post.
func (app *App) quoteRevoked(actor *Account, iri string) bool {
	res, err := app.db.Exec(`UPDATE statuses SET quote_state='revoked', quote_auth='' WHERE quote_auth=? AND quote_uri IN
		(SELECT uri FROM statuses WHERE account_id=?)`, iri, actor.ID)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// RevokeQuote withdraws the approval of quoting for one of my posts.
func (app *App) RevokeQuote(me *Account, quoted, quoting *Status) error {
	if quoted.AccountID != me.ID || quoting.QuoteURI != quoted.URI {
		return errNotFound
	}
	var id string
	if app.db.QueryRow(`SELECT id FROM quote_authorizations WHERE status_id=? AND quoting_uri=?`, quoted.ID, quoting.URI).Scan(&id) == nil {
		app.db.Exec(`DELETE FROM quote_authorizations WHERE id=?`, id)
	}
	app.db.Exec(`UPDATE statuses SET quote_state='revoked', quote_auth='' WHERE id=?`, quoting.ID)
	app.db.Exec(`DELETE FROM notifications WHERE type='quote' AND status_id=? AND account_id=?`, quoting.ID, me.ID)
	app.publishStatus(quoting.ID, true)
	if id != "" {
		author, err := app.AccountByID(quoting.AccountID)
		if err == nil && !author.IsLocal() {
			stamp := app.quoteAuthURI(me, id)
			app.enqueue(me, append(app.followerInboxes(me.ID), author.DeliveryInbox()), withContext(obj{
				"id": stamp + "#delete", "type": "Delete", "actor": me.URI, "object": stamp,
				"to": []any{asPublic},
			}))
		}
	}
	return nil
}

// SetQuotePolicy changes who may quote one of my posts and tells the
// servers that have it.
func (app *App) SetQuotePolicy(me *Account, st *Status, policy string) error {
	if st.AccountID != me.ID || !st.Local || !quotePolicies[policy] {
		return errors.New("not your post")
	}
	if _, err := app.db.Exec(`UPDATE statuses SET quote_policy=? WHERE id=?`, policy, st.ID); err != nil {
		return err
	}
	full, err := app.loadStatus(me, st.ID)
	if err != nil {
		return err
	}
	app.enqueue(me, app.statusInboxes(full), app.updateNoteActivity(full))
	app.publishStatus(st.ID, true)
	return nil
}

// handleQuoteAuthorization serves a stamp, so other servers can check a quote.
func (app *App) handleQuoteAuthorization(w http.ResponseWriter, r *http.Request) {
	acc, err := app.LocalAccount(r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var statusID, quotingURI string
	if app.db.QueryRow(`SELECT status_id, quoting_uri FROM quote_authorizations WHERE id=?`, r.PathValue("id")).Scan(&statusID, &quotingURI) != nil {
		http.NotFound(w, r)
		return
	}
	st, err := app.StatusByID(statusID)
	if err != nil || st.AccountID != acc.ID {
		http.NotFound(w, r)
		return
	}
	writeAP(w, obj{
		"@context": []any{"https://www.w3.org/ns/activitystreams", obj{
			"gts":                "https://gotosocial.org/ns#",
			"QuoteAuthorization": "https://w3id.org/fep/044f#QuoteAuthorization",
			"interactingObject":  obj{"@id": "gts:interactingObject", "@type": "@id"},
			"interactionTarget":  obj{"@id": "gts:interactionTarget", "@type": "@id"},
		}},
		"id":                app.quoteAuthURI(acc, r.PathValue("id")),
		"type":              "QuoteAuthorization",
		"attributedTo":      acc.URI,
		"interactingObject": quotingURI,
		"interactionTarget": st.URI,
	})
}
