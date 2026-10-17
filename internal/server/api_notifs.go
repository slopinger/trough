package server

import (
	"net/http"
	"strings"
	"time"

	"git.freifick.net/slop/trough/internal/ap"
	"git.freifick.net/slop/trough/internal/ulid"
)

// Grouped notifications (Mastodon 4.3, /api/v2/notifications). Likes and
// boosts of the same post, and follows on the same day, collapse into one
// group; everything else is a group of one. Group keys:
//   favourite-<status id>, reblog-<status id>, follow-<yyyymmdd>, ungrouped-<notification id>

var defaultGroupedTypes = map[string]bool{"favourite": true, "reblog": true, "follow": true}

func groupKey(n *Notification, grouped map[string]bool) string {
	t := notificationTypes[n.Type]
	switch {
	case !grouped[t]:
	case (t == "favourite" || t == "reblog") && n.Status != nil:
		return t + "-" + n.Status.ID
	case t == "follow":
		return "follow-" + n.CreatedAt.UTC().Format("20060102")
	}
	return "ungrouped-" + n.ID
}

// groupWhere selects the notifications in a group.
func groupWhere(key string) (string, []any, bool) {
	kind, rest, _ := strings.Cut(key, "-")
	switch kind {
	case "ungrouped":
		return `n.id=?`, []any{rest}, true
	case "favourite", "reblog":
		return `n.type=? AND n.status_id=?`, []any{kind, rest}, true
	case "follow":
		day, err := time.Parse("20060102", rest)
		if err != nil {
			return "", nil, false
		}
		return `n.type='follow' AND n.created_at >= ? AND n.created_at < ?`, []any{day.Unix(), day.Add(24 * time.Hour).Unix()}, true
	}
	return "", nil, false
}

func groupedTypes(r *http.Request) map[string]bool {
	q := r.URL.Query()
	gt := append(q["grouped_types[]"], q["grouped_types"]...)
	if len(gt) == 0 {
		return defaultGroupedTypes
	}
	out := map[string]bool{}
	for _, t := range gt {
		out[t] = true
	}
	return out
}

// groupedResponse builds {accounts, statuses, notification_groups} from
// notifications in newest-first order.
func (app *App) groupedResponse(me *Account, ns []*Notification, grouped map[string]bool) obj {
	accounts, statuses, groups := []any{}, []any{}, []any{}
	seenAcc, seenSt := map[string]bool{}, map[string]bool{}
	index := map[string]obj{}
	for _, n := range ns {
		key := groupKey(n, grouped)
		g := index[key]
		if g == nil {
			g = obj{"group_key": key, "notifications_count": 0, "type": notificationTypes[n.Type],
				"most_recent_notification_id": notificationNumber(n.ID), "page_max_id": n.ID,
				"latest_page_notification_at": apiTime(n.CreatedAt), "sample_account_ids": []string{}}
			if n.Status != nil {
				g["status_id"] = n.Status.ID
				if !seenSt[n.Status.ID] {
					seenSt[n.Status.ID] = true
					statuses = append(statuses, app.apiStatus(me, n.Status))
				}
			}
			if n.Type == "reaction" && n.Status != nil {
				for _, r := range n.Status.Reactions {
					g["emoji"] = r.Emoji
					break
				}
			}
			if n.Warning != nil {
				g["moderation_warning"] = app.apiWarning(n.Warning)
			}
			if n.Event != nil {
				g["event"] = app.apiSeverance(n.Event)
			}
			index[key] = g
			groups = append(groups, g)
		}
		g["notifications_count"] = g["notifications_count"].(int) + 1
		g["page_min_id"] = n.ID
		if ids := g["sample_account_ids"].([]string); len(ids) < 8 && !contains(ids, n.From.ID) {
			g["sample_account_ids"] = append(ids, n.From.ID)
		}
		if !seenAcc[n.From.ID] {
			seenAcc[n.From.ID] = true
			accounts = append(accounts, app.apiAccount(n.From))
		}
	}
	// counts cover the whole group, not just this page
	for _, g := range index {
		if where, args, ok := groupWhere(ap.Str(g, "group_key")); ok && !strings.HasPrefix(ap.Str(g, "group_key"), "ungrouped-") {
			g["notifications_count"] = app.countInt(`SELECT COUNT(*) FROM notifications n WHERE n.account_id=? AND `+where, append([]any{me.ID}, args...)...)
		}
	}
	return obj{"accounts": accounts, "statuses": statuses, "notification_groups": groups}
}

func (app *App) apiNotificationsV2(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	pq := pageParams(r, 40, 80)
	numericPageBounds(&pq)
	where, args, order, none := app.notificationFilter(r, c, pq)
	if none {
		apiJSON(w, 200, obj{"accounts": []any{}, "statuses": []any{}, "notification_groups": []any{}})
		return
	}
	ns, err := app.notificationsWhere(c.Me, where, args, order, pq.Limit)
	if err != nil {
		apiError(w, 500, "db error")
		return
	}
	if order == "ASC" {
		for i, j := 0, len(ns)-1; i < j; i, j = i+1, j-1 {
			ns[i], ns[j] = ns[j], ns[i]
		}
	}
	if len(ns) > 0 {
		app.setLink(w, r, ns[0].ID, ns[len(ns)-1].ID, len(ns) == pq.Limit)
	}
	apiJSON(w, 200, app.groupedResponse(c.Me, ns, groupedTypes(r)))
}

func (app *App) notificationGroup(r *http.Request, c *apiCtx) ([]*Notification, bool) {
	where, args, ok := groupWhere(r.PathValue("key"))
	if !ok {
		return nil, false
	}
	ns, err := app.notificationsWhere(c.Me, `n.account_id=? AND `+where, append([]any{c.Me.ID}, args...), "DESC", 200)
	return ns, err == nil && len(ns) > 0
}

func (app *App) apiNotificationGroup(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	ns, ok := app.notificationGroup(r, c)
	if !ok {
		apiError(w, 404, "Record not found")
		return
	}
	apiJSON(w, 200, app.groupedResponse(c.Me, ns, defaultGroupedTypes))
}

func (app *App) apiNotificationGroupAccounts(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	ns, ok := app.notificationGroup(r, c)
	if !ok {
		apiError(w, 404, "Record not found")
		return
	}
	out, seen := []any{}, map[string]bool{}
	for _, n := range ns {
		if !seen[n.From.ID] {
			seen[n.From.ID] = true
			out = append(out, app.apiAccount(n.From))
		}
	}
	apiJSON(w, 200, out)
}

func (app *App) apiDismissNotificationGroup(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if where, args, ok := groupWhere(r.PathValue("key")); ok {
		app.db.Exec(`DELETE FROM notifications AS n WHERE n.account_id=? AND `+where, append([]any{c.Me.ID}, args...)...)
	}
	apiJSON(w, 200, obj{})
}

func (app *App) apiEmptyList(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	apiJSON(w, 200, []any{})
}

// notificationNumber is most_recent_notification_id: a number on Mastodon,
// and Ice Cubes can't read it otherwise. It also works as max_id/since_id.
func notificationNumber(id string) any {
	if n, ok := ulid.Number(id); ok {
		return n
	}
	return id
}

// notificationFilter turns the Mastodon query parameters shared by the v1
// and v2 notification endpoints into SQL. none means nothing can match.
func (app *App) notificationFilter(r *http.Request, c *apiCtx, pq pageQ) (where string, args []any, order string, none bool) {
	q := r.URL.Query()
	where = `n.account_id=? AND (a.suspended=0 OR n.type='signup') AND NOT EXISTS(SELECT 1 FROM mutes mu WHERE mu.account_id=n.account_id AND mu.target_id=n.from_account_id AND mu.notifications=1) AND ` + notifDomainBlockSQL
	args = []any{c.Me.ID}
	if q.Get("include_filtered") != "true" {
		where += ` AND n.filtered=0`
	}
	reverse := map[string]string{}
	for k, v := range notificationTypes {
		reverse[v] = k
	}
	if types := append(q["types[]"], q["types"]...); len(types) > 0 {
		var ours []string
		for _, t := range types {
			if o, ok := reverse[t]; ok {
				ours = append(ours, o)
			}
		}
		if len(ours) == 0 {
			return "", nil, "", true
		}
		ph, a := inClause(`%s`, ours)
		where += ` AND n.type IN (` + ph + `)`
		args = append(args, a...)
	}
	if ex := append(q["exclude_types[]"], q["exclude_types"]...); len(ex) > 0 {
		var ours []string
		for _, t := range ex {
			if o, ok := reverse[t]; ok {
				ours = append(ours, o)
			}
		}
		if len(ours) > 0 {
			ph, a := inClause(`%s`, ours)
			where += ` AND n.type NOT IN (` + ph + `)`
			args = append(args, a...)
		}
	}
	if id := q.Get("account_id"); id != "" {
		where += ` AND n.from_account_id=?`
		args = append(args, id)
	}
	order = "DESC"
	if pq.MaxID != "" {
		where += ` AND n.id < ?`
		args = append(args, pq.MaxID)
	}
	if pq.SinceID != "" {
		where += ` AND n.id > ?`
		args = append(args, pq.SinceID)
	}
	if pq.MinID != "" {
		where += ` AND n.id > ?`
		args = append(args, pq.MinID)
		order = "ASC"
	}
	return where, args, order, false
}

func (app *App) apiNotifications(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	pq := pageParams(r, 15, 80)
	numericPageBounds(&pq)
	where, args, order, none := app.notificationFilter(r, c, pq)
	if none {
		apiJSON(w, 200, []any{})
		return
	}
	ns, err := app.notificationsWhere(c.Me, where, args, order, pq.Limit)
	if err != nil {
		apiError(w, 500, "db error")
		return
	}
	out := []any{}
	for _, n := range app.filterNotifications(c.Me, ns) {
		out = append(out, app.apiNotification(c.Me, n))
	}
	if len(ns) > 0 {
		app.setLink(w, r, ns[0].ID, ns[len(ns)-1].ID, len(ns) == pq.Limit)
	}
	apiJSON(w, 200, out)
}

// filterNotifications drops notifications about posts hidden by a filter
// for the "notifications" context and marks those to warn about.
func (app *App) filterNotifications(me *Account, ns []*Notification) []*Notification {
	var sts []*Status
	for _, n := range ns {
		if n.Status != nil {
			sts = append(sts, n.Status)
		}
	}
	keep := map[*Status]bool{}
	for _, s := range app.applyFilters(me, sts, "notifications") {
		keep[s] = true
	}
	out := ns[:0]
	for _, n := range ns {
		if n.Status == nil || keep[n.Status] {
			out = append(out, n)
		}
	}
	return out
}

func (app *App) apiNotification1(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	ns, err := app.notificationsWhere(c.Me, `n.account_id=? AND n.id=?`, []any{c.Me.ID, r.PathValue("id")}, "DESC", 1)
	if err != nil || len(ns) == 0 {
		apiError(w, 404, "Record not found")
		return
	}
	apiJSON(w, 200, app.apiNotification(c.Me, ns[0]))
}

func (app *App) apiClearNotifications(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	app.db.Exec(`DELETE FROM notifications WHERE account_id=?`, c.Me.ID)
	apiJSON(w, 200, obj{})
}

func (app *App) apiDismissNotification(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	app.db.Exec(`DELETE FROM notifications WHERE account_id=? AND id=?`, c.Me.ID, r.PathValue("id"))
	apiJSON(w, 200, obj{})
}

func (app *App) apiUnreadCount(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	apiJSON(w, 200, obj{"count": app.unreadNotifications(c.Me.ID)})
}
