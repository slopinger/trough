package server

import (
	"net/http"
	"strconv"
	"time"
)

// Who to follow: accounts followed by people you follow, then the most
// followed accounts on this server, without those you already follow,
// requested, blocked, muted or dismissed. And "familiar followers": the
// people you follow who follow an account.

// Suggestion is an account to follow and why.
type Suggestion struct {
	Account *Account
	Source  string // "friends_of_friends" or "global" (Mastodon's v2 sources)
}

func (app *App) suggestions(me *Account, limit int) []Suggestion {
	exclude := `a.id != ? AND a.suspended=0 AND a.limited=0 AND a.discoverable=1 AND a.moved_to='' AND a.actor_type IN ('Person','Service')
		AND NOT EXISTS(SELECT 1 FROM follows x WHERE x.account_id=? AND x.target_id=a.id)
		AND NOT EXISTS(SELECT 1 FROM blocks x WHERE (x.account_id=? AND x.target_id=a.id) OR (x.account_id=a.id AND x.target_id=?))
		AND NOT EXISTS(SELECT 1 FROM mutes x WHERE x.account_id=? AND x.target_id=a.id)
		AND NOT EXISTS(SELECT 1 FROM suggestion_dismissals x WHERE x.account_id=? AND x.target_id=a.id)
		AND NOT EXISTS(SELECT 1 FROM user_domain_blocks x WHERE x.account_id=? AND x.domain=a.domain)`
	ex := []any{me.ID, me.ID, me.ID, me.ID, me.ID, me.ID, me.ID}
	var out []Suggestion
	seen := map[string]bool{}
	add := func(accs []*Account, source string) {
		for _, a := range accs {
			if !seen[a.ID] && len(out) < limit {
				seen[a.ID] = true
				out = append(out, Suggestion{a, source})
			}
		}
	}
	fof, _ := app.accountsQuery(`SELECT `+accountCols+` FROM follows f2 JOIN follows f1 ON f1.target_id=f2.account_id AND f1.account_id=? AND f1.accepted=1
		JOIN accounts a ON a.id=f2.target_id WHERE f2.accepted=1
		AND NOT EXISTS(SELECT 1 FROM accounts h WHERE h.id=f2.account_id AND h.hide_collections=1) AND `+exclude+`
		GROUP BY a.id ORDER BY COUNT(*) DESC, MAX(f2.created_at) DESC LIMIT ?`, append(append([]any{me.ID}, ex...), limit)...)
	add(fof, "friends_of_friends")
	if len(out) < limit {
		// accounts here (or followed from here) that people follow and that posted lately
		popular, _ := app.accountsQuery(`SELECT `+accountCols+` FROM accounts a JOIN follows f ON f.target_id=a.id AND f.accepted=1
			WHERE `+exclude+` AND EXISTS(SELECT 1 FROM statuses s WHERE s.account_id=a.id AND s.visibility='public' AND s.created_at > ?)
			GROUP BY a.id ORDER BY a.domain != '', COUNT(*) DESC LIMIT ?`, append(ex, time.Now().AddDate(0, -1, 0).Unix(), limit)...)
		add(popular, "global")
	}
	return out
}

func (app *App) dismissSuggestion(me *Account, targetID string) {
	app.db.Exec(`INSERT OR IGNORE INTO suggestion_dismissals (account_id, target_id, created_at) SELECT ?, id, ? FROM accounts WHERE id=?`,
		me.ID, time.Now().Unix(), targetID)
}

// familiarFollowers returns accounts that me follows and that follow target.
func (app *App) familiarFollowers(me, target *Account, limit int) []*Account {
	accs, _ := app.accountsQuery(`SELECT `+accountCols+` FROM follows mine JOIN follows theirs ON theirs.account_id=mine.target_id
		JOIN accounts a ON a.id=mine.target_id WHERE mine.account_id=? AND mine.accepted=1 AND theirs.target_id=? AND theirs.accepted=1
		AND a.suspended=0 AND a.hide_collections=0 AND NOT EXISTS(SELECT 1 FROM accounts t WHERE t.id=theirs.target_id AND t.hide_collections=1) ORDER BY mine.created_at DESC LIMIT ?`, me.ID, target.ID, limit)
	return accs
}

func suggestionLimit(r *http.Request) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		return min(n, 80)
	}
	return 40
}

func (app *App) apiSuggestions(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var accs []*Account
	for _, s := range app.suggestions(c.Me, suggestionLimit(r)) {
		accs = append(accs, s.Account)
	}
	app.writeAccounts(w, accs)
}

func (app *App) apiSuggestionsV2(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	for _, s := range app.suggestions(c.Me, suggestionLimit(r)) {
		legacy := "global"
		if s.Source == "friends_of_friends" {
			legacy = "past_interactions"
		}
		out = append(out, obj{"source": legacy, "sources": []string{s.Source}, "account": app.apiAccount(s.Account)})
	}
	apiJSON(w, 200, out)
}

func (app *App) apiDismissSuggestion(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	app.dismissSuggestion(c.Me, r.PathValue("id"))
	apiJSON(w, 200, obj{})
}

func (app *App) apiFamiliarFollowers(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	q := r.URL.Query()
	out := []any{}
	for _, id := range append(q["id[]"], q["id"]...) {
		accs := []any{}
		if target, err := app.AccountByID(id); err == nil {
			for _, a := range app.familiarFollowers(c.Me, target, 40) {
				accs = append(accs, app.apiAccount(a))
			}
		}
		out = append(out, obj{"id": id, "accounts": accs})
	}
	apiJSON(w, 200, out)
}
