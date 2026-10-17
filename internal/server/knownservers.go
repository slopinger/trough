package server

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/slopinger/trough/internal/ap"
)

// The admin list of known servers (/admin/servers, like Mastodon's
// admin instances): every domain we have accounts from, with how connected
// it is to this server, how lively, whether deliveries to it fail and
// whether it's moderated. Computed from the stored accounts, posts, follows
// and delivery queue.

type KnownServer struct {
	Domain               string
	Accounts, Posts      int
	Following, Followers int // follows from local accounts to it, and from it to local accounts
	LastPost             time.Time
	FailingDeliveries    int
	Severity             string // "", limit or suspend
}

const serversPerPage = 50

func (app *App) knownServers() []*KnownServer {
	byDomain := map[string]*KnownServer{}
	get := func(d string) *KnownServer {
		if byDomain[d] == nil {
			byDomain[d] = &KnownServer{Domain: d}
		}
		return byDomain[d]
	}
	grouped := func(q string, add func(*KnownServer, int, int64)) {
		rows, err := app.db.Query(q)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var d string
			var n int
			var last int64
			if rows.Scan(&d, &n, &last) == nil && d != "" {
				add(get(d), n, last)
			}
		}
	}
	grouped(`SELECT domain, COUNT(*), 0 FROM accounts WHERE domain != '' GROUP BY domain`,
		func(s *KnownServer, n int, _ int64) { s.Accounts = n })
	grouped(`SELECT a.domain, COUNT(*), MAX(s.created_at) FROM statuses s JOIN accounts a ON a.id=s.account_id WHERE s.local=0 GROUP BY a.domain`,
		func(s *KnownServer, n int, last int64) { s.Posts, s.LastPost = n, time.Unix(last, 0) })
	grouped(`SELECT t.domain, COUNT(*), 0 FROM follows f JOIN accounts t ON t.id=f.target_id JOIN accounts l ON l.id=f.account_id
		WHERE l.domain='' AND t.domain != '' AND f.accepted=1 GROUP BY t.domain`,
		func(s *KnownServer, n int, _ int64) { s.Following = n })
	grouped(`SELECT a.domain, COUNT(*), 0 FROM follows f JOIN accounts a ON a.id=f.account_id JOIN accounts l ON l.id=f.target_id
		WHERE l.domain='' AND a.domain != '' AND f.accepted=1 GROUP BY a.domain`,
		func(s *KnownServer, n int, _ int64) { s.Followers = n })
	if rows, err := app.db.Query(`SELECT inbox FROM deliveries WHERE attempts > 0`); err == nil {
		for rows.Next() {
			var inbox string
			if rows.Scan(&inbox) == nil {
				if s := byDomain[ap.Host(inbox)]; s != nil {
					s.FailingDeliveries++
				}
			}
		}
		rows.Close()
	}
	for _, b := range app.domainBlockList() {
		for d, s := range byDomain {
			if d == b.Domain || strings.HasSuffix(d, "."+b.Domain) {
				s.Severity = b.Severity
			}
		}
	}
	out := make([]*KnownServer, 0, len(byDomain))
	for _, s := range byDomain {
		out = append(out, s)
	}
	return out
}

// sortServers orders by connections (default), posts, recent activity or name.
func sortServers(list []*KnownServer, by string) {
	slices.SortFunc(list, func(a, b *KnownServer) int {
		var c int
		switch by {
		case "posts":
			c = cmp.Compare(b.Posts, a.Posts)
		case "recent":
			c = b.LastPost.Compare(a.LastPost)
		case "name":
		default:
			c = cmp.Compare(b.Following+b.Followers, a.Following+a.Followers)
		}
		return cmp.Or(c, strings.Compare(a.Domain, b.Domain))
	})
}

// GET /admin/servers?q=&sort=&page=
func (app *App) handleAdminServers(w http.ResponseWriter, r *http.Request, p *Page) {
	p.Title, p.Section = "Known servers", "admin"
	list := app.knownServers()
	p.Query = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if p.Query != "" {
		list = slices.DeleteFunc(list, func(s *KnownServer) bool { return !strings.Contains(s.Domain, p.Query) })
	}
	p.Tab = r.URL.Query().Get("sort")
	sortServers(list, p.Tab)
	p.Stats = map[string]int{"total": len(list)}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	from := min((page-1)*serversPerPage, len(list))
	p.Servers = list[from:min(from+serversPerPage, len(list))]
	if from+serversPerPage < len(list) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		p.NextURL = "/admin/servers?" + q.Encode()
	}
	app.render(w, "adminservers", p)
}
