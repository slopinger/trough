package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Notification filtering, as in Mastodon 4.3: each user decides what
// happens to notifications from people they don't follow, people who don't
// follow them, new accounts and unsolicited private mentions: accept,
// filter (kept aside as a "notification request" per sender, to accept or
// dismiss) or drop. Accepting a request lets that sender through from then on.

// NotificationPolicy is a user's policy; each field is accept, filter or drop.
type NotificationPolicy struct {
	NotFollowing    string `json:"for_not_following"`
	NotFollowers    string `json:"for_not_followers"`
	NewAccounts     string `json:"for_new_accounts"`
	PrivateMentions string `json:"for_private_mentions"`
	LimitedAccounts string `json:"for_limited_accounts"`
}

var defaultNotificationPolicy = NotificationPolicy{"accept", "accept", "accept", "filter", "filter"}

var policyValues = map[string]int{"accept": 0, "filter": 1, "drop": 2}

// notifications of these types are never filtered (as on Mastodon)
var unfilteredTypes = map[string]bool{"poll": true, "update": true, "quoted_update": true, "follow_request": true, "admin.report": true, "admin.sign_up": true}

func (app *App) notificationPolicy(accountID string) NotificationPolicy {
	p := defaultNotificationPolicy
	var raw string
	app.db.QueryRow(`SELECT notification_policy FROM users WHERE account_id=?`, accountID).Scan(&raw)
	json.Unmarshal([]byte(raw), &p)
	for _, v := range []*string{&p.NotFollowing, &p.NotFollowers, &p.NewAccounts, &p.PrivateMentions, &p.LimitedAccounts} {
		if _, ok := policyValues[*v]; !ok {
			*v = "accept"
		}
	}
	return p
}

func (app *App) setNotificationPolicy(accountID string, p NotificationPolicy) {
	b, _ := json.Marshal(p)
	app.db.Exec(`UPDATE users SET notification_policy=? WHERE account_id=?`, string(b), accountID)
}

// notificationDecision is what recipient's policy says about a notification from from.
func (app *App) notificationDecision(recipientID, typ string, from *Account, statusID string) string {
	if unfilteredTypes[typ] || app.db.QueryRow(`SELECT 1 FROM notification_accepts WHERE account_id=? AND from_account_id=?`,
		recipientID, from.ID).Scan(new(int)) == nil {
		return "accept"
	}
	p := app.notificationPolicy(recipientID)
	rel := app.relationship(recipientID, from.ID)
	worst := "accept"
	consider := func(v string) {
		if policyValues[v] > policyValues[worst] {
			worst = v
		}
	}
	if !rel.Following {
		consider(p.NotFollowing)
		if from.IsLimited() {
			consider(p.LimitedAccounts)
		}
	}
	followsSince := int64(0)
	app.db.QueryRow(`SELECT created_at FROM follows WHERE account_id=? AND target_id=? AND accepted=1`, from.ID, recipientID).Scan(&followsSince)
	if followsSince == 0 || time.Since(time.Unix(followsSince, 0)) < 3*24*time.Hour {
		consider(p.NotFollowers)
	}
	if time.Since(from.CreatedAt) < 30*24*time.Hour {
		consider(p.NewAccounts)
	}
	if typ == "mention" && statusID != "" && !rel.Following {
		var vis, replyTo string
		app.db.QueryRow(`SELECT visibility, in_reply_to_account_id FROM statuses WHERE id=?`, statusID).Scan(&vis, &replyTo)
		if vis == "direct" && replyTo != recipientID {
			consider(p.PrivateMentions)
		}
	}
	return worst
}

// NotificationRequest groups the filtered notifications from one account.
type NotificationRequest struct {
	Account          *Account
	Count            int
	Created, Updated time.Time
	LastStatus       *Status
	lastStatusID     string
	lastNotif        string
}

func (app *App) notificationRequests(me *Account, fromID string) []*NotificationRequest {
	q := `SELECT from_account_id, COUNT(*), MIN(created_at), MAX(created_at), MAX(id),
		COALESCE((SELECT status_id FROM notifications x WHERE x.account_id=n.account_id AND x.from_account_id=n.from_account_id AND x.filtered=1 AND x.status_id != '' ORDER BY x.id DESC LIMIT 1), '')
		FROM notifications n WHERE account_id=? AND filtered=1`
	args := []any{me.ID}
	if fromID != "" {
		q += ` AND from_account_id=?`
		args = append(args, fromID)
	}
	rows, err := app.db.Query(q+` GROUP BY from_account_id ORDER BY MAX(id) DESC LIMIT 80`, args...)
	if err != nil {
		return nil
	}
	var out []*NotificationRequest
	var froms []string
	for rows.Next() {
		r := &NotificationRequest{}
		var from string
		var c, u int64
		rows.Scan(&from, &r.Count, &c, &u, &r.lastNotif, &r.lastStatusID)
		r.Created, r.Updated = time.Unix(c, 0), time.Unix(u, 0)
		froms = append(froms, from)
		out = append(out, r)
	}
	rows.Close()
	keep := out[:0]
	for i, r := range out {
		if acc, err := app.AccountByID(froms[i]); err == nil {
			r.Account = acc
			if r.lastStatusID != "" {
				r.LastStatus, _ = app.loadStatus(me, r.lastStatusID)
			}
			keep = append(keep, r)
		}
	}
	return keep
}

func (app *App) acceptNotificationRequest(me *Account, fromID string) {
	app.db.Exec(`UPDATE notifications SET filtered=0 WHERE account_id=? AND from_account_id=? AND filtered=1`, me.ID, fromID)
	forgetUnreadDMs(me.ID)
	app.db.Exec(`INSERT OR IGNORE INTO notification_accepts (account_id, from_account_id, created_at) SELECT ?, id, ? FROM accounts WHERE id=?`,
		me.ID, time.Now().Unix(), fromID)
}

func (app *App) dismissNotificationRequest(me *Account, fromID string) {
	for _, c := range app.conversations(me) {
		if c.Filtered && c.Last.AccountID == fromID {
			app.markConversation(me, c, "hidden_id")
		}
	}
	app.db.Exec(`DELETE FROM notifications WHERE account_id=? AND from_account_id=? AND filtered=1`, me.ID, fromID)
}

func (app *App) pendingNotificationCounts(me *Account) (requests, notifications int) {
	app.db.QueryRow(`SELECT COUNT(DISTINCT from_account_id), COUNT(*) FROM notifications WHERE account_id=? AND filtered=1`, me.ID).
		Scan(&requests, &notifications)
	return
}

// ---- API ----

func (app *App) apiNotificationPolicy(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if r.Method != http.MethodGet {
		p := app.notificationPolicy(c.Me.ID)
		params := readParams(r)
		for key, field := range map[string]*string{"for_not_following": &p.NotFollowing, "for_not_followers": &p.NotFollowers,
			"for_new_accounts": &p.NewAccounts, "for_private_mentions": &p.PrivateMentions, "for_limited_accounts": &p.LimitedAccounts} {
			if v := params.Get(key); v != "" {
				if _, ok := policyValues[v]; !ok {
					apiError(w, 422, "Validation failed: "+key+" must be accept, filter or drop")
					return
				}
				*field = v
			}
		}
		// v1 names: filter_* booleans
		for key, field := range map[string]*string{"filter_not_following": &p.NotFollowing, "filter_not_followers": &p.NotFollowers,
			"filter_new_accounts": &p.NewAccounts, "filter_private_mentions": &p.PrivateMentions} {
			if params.Has(key) {
				*field = map[bool]string{true: "filter", false: "accept"}[params.Bool(key)]
			}
		}
		app.setNotificationPolicy(c.Me.ID, p)
	}
	p := app.notificationPolicy(c.Me.ID)
	reqs, notifs := app.pendingNotificationCounts(c.Me)
	apiJSON(w, 200, obj{
		"for_not_following": p.NotFollowing, "for_not_followers": p.NotFollowers, "for_new_accounts": p.NewAccounts,
		"for_private_mentions": p.PrivateMentions, "for_limited_accounts": p.LimitedAccounts,
		"filter_not_following": p.NotFollowing != "accept", "filter_not_followers": p.NotFollowers != "accept",
		"filter_new_accounts": p.NewAccounts != "accept", "filter_private_mentions": p.PrivateMentions != "accept",
		"summary": obj{"pending_requests_count": reqs, "pending_notifications_count": notifs},
	})
}

func (app *App) apiNotificationRequest(c *apiCtx, req *NotificationRequest) obj {
	var last any
	if req.LastStatus != nil {
		last = app.apiStatus(c.Me, req.LastStatus)
	}
	return obj{"id": req.Account.ID, "created_at": apiTime(req.Created), "updated_at": apiTime(req.Updated),
		"account": app.apiAccount(req.Account), "notifications_count": strconv.Itoa(req.Count), "last_status": last}
}

func (app *App) apiNotificationRequests(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	out := []any{}
	for _, req := range app.notificationRequests(c.Me, "") {
		out = append(out, app.apiNotificationRequest(c, req))
	}
	apiJSON(w, 200, out)
}

func (app *App) apiNotificationRequest1(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	reqs := app.notificationRequests(c.Me, r.PathValue("id"))
	if len(reqs) == 0 {
		apiError(w, 404, "Record not found")
		return
	}
	switch r.PathValue("action") {
	case "":
		apiJSON(w, 200, app.apiNotificationRequest(c, reqs[0]))
		return
	case "accept":
		app.acceptNotificationRequest(c.Me, reqs[0].Account.ID)
	case "dismiss":
		app.dismissNotificationRequest(c.Me, reqs[0].Account.ID)
	default:
		apiError(w, 404, "Record not found")
		return
	}
	apiJSON(w, 200, obj{})
}

// apiNotificationRequestsBulk handles POST .../requests/accept and /dismiss with id[].
func (app *App) apiNotificationRequestsBulk(accept bool) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, c *apiCtx) {
		for _, id := range readParams(r).List("id") {
			if accept {
				app.acceptNotificationRequest(c.Me, id)
			} else {
				app.dismissNotificationRequest(c.Me, id)
			}
		}
		apiJSON(w, 200, obj{})
	}
}

// ---- web UI ----

func (app *App) handleNotificationRequests(w http.ResponseWriter, r *http.Request, p *Page) {
	if r.Method == http.MethodPost {
		from := r.FormValue("account")
		if r.FormValue("action") == "accept" {
			app.acceptNotificationRequest(p.Me, from)
			setFlash(w, "Accepted. Their notifications show up normally from now on.")
		} else {
			app.dismissNotificationRequest(p.Me, from)
		}
		http.Redirect(w, r, "/notifications/requests", http.StatusSeeOther)
		return
	}
	p.Title, p.Section = "Filtered notifications", "notifications"
	p.Requests2 = app.notificationRequests(p.Me, "")
	app.render(w, "notifrequests", p)
}

func (app *App) handleNotificationPolicy(w http.ResponseWriter, r *http.Request, p *Page) {
	pol := app.notificationPolicy(p.Me.ID)
	for key, field := range map[string]*string{"not_following": &pol.NotFollowing, "not_followers": &pol.NotFollowers,
		"new_accounts": &pol.NewAccounts, "private_mentions": &pol.PrivateMentions, "limited_accounts": &pol.LimitedAccounts} {
		if v := r.FormValue(key); policyValues[v] > 0 || v == "accept" {
			*field = v
		}
	}
	app.setNotificationPolicy(p.Me.ID, pol)
	setFlash(w, "Notification filters saved.")
	http.Redirect(w, r, "/settings#notification-filters", http.StatusSeeOther)
}
