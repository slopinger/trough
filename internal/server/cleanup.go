package server

import (
	"log"
	"net/http"
	"strconv"
	"time"
)

// Automated post deletion, like Mastodon's: a user can have their posts
// deleted once they reach an age, except those they want to keep. Deletion
// federates like a manual delete, so it runs in small batches.

const cleanupBatch = 20 // posts per account every ten minutes

// CleanupPolicy is a user's automated post deletion setting.
type CleanupPolicy struct {
	Enabled                 bool
	MinAge                  int64 // seconds
	KeepDirect, KeepPinned  bool
	KeepPolls, KeepMedia    bool
	KeepSelfFav, KeepSelfBM bool
	MinFavs, MinBoosts      int // keep posts with at least this many; 0: no limit
}

// cleanupAges are the offered ages, Mastodon's.
var cleanupAges = []struct {
	Seconds int64
	Label   string
}{
	{7 * 86400, "1 week"}, {14 * 86400, "2 weeks"}, {30 * 86400, "1 month"}, {60 * 86400, "2 months"},
	{90 * 86400, "3 months"}, {180 * 86400, "6 months"}, {365 * 86400, "1 year"}, {730 * 86400, "2 years"},
}

func defaultCleanupPolicy() CleanupPolicy {
	return CleanupPolicy{MinAge: 14 * 86400, KeepDirect: true, KeepPinned: true, KeepSelfFav: true, KeepSelfBM: true}
}

func (app *App) cleanupPolicy(accountID string) CleanupPolicy {
	p := defaultCleanupPolicy()
	app.db.QueryRow(`SELECT enabled, min_age, keep_direct, keep_pinned, keep_polls, keep_media, keep_self_fav, keep_self_bookmark, min_favs, min_boosts
		FROM cleanup_policies WHERE account_id=?`, accountID).Scan(&p.Enabled, &p.MinAge, &p.KeepDirect, &p.KeepPinned, &p.KeepPolls,
		&p.KeepMedia, &p.KeepSelfFav, &p.KeepSelfBM, &p.MinFavs, &p.MinBoosts)
	return p
}

func (app *App) setCleanupPolicy(accountID string, p CleanupPolicy) {
	known := false
	for _, a := range cleanupAges {
		known = known || a.Seconds == p.MinAge
	}
	if !known {
		p.MinAge = defaultCleanupPolicy().MinAge
	}
	p.MinFavs, p.MinBoosts = max(p.MinFavs, 0), max(p.MinBoosts, 0)
	app.db.Exec(`INSERT INTO cleanup_policies (account_id, enabled, min_age, keep_direct, keep_pinned, keep_polls, keep_media, keep_self_fav, keep_self_bookmark, min_favs, min_boosts)
		VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT (account_id) DO UPDATE SET enabled=excluded.enabled, min_age=excluded.min_age,
		keep_direct=excluded.keep_direct, keep_pinned=excluded.keep_pinned, keep_polls=excluded.keep_polls, keep_media=excluded.keep_media,
		keep_self_fav=excluded.keep_self_fav, keep_self_bookmark=excluded.keep_self_bookmark, min_favs=excluded.min_favs, min_boosts=excluded.min_boosts`,
		accountID, p.Enabled, p.MinAge, p.KeepDirect, p.KeepPinned, p.KeepPolls, p.KeepMedia, p.KeepSelfFav, p.KeepSelfBM, p.MinFavs, p.MinBoosts)
}

// where selects the account's posts the policy would delete now.
func (p CleanupPolicy) where(accountID string, now time.Time) (string, []any) {
	q := `s.account_id=? AND s.created_at < ?`
	args := []any{accountID, now.Unix() - p.MinAge}
	// a boost is kept or not by what happened to the boosted post
	const target = `CASE WHEN s.boost_of_id != '' THEN s.boost_of_id ELSE s.id END`
	if p.KeepDirect {
		q += ` AND s.visibility != 'direct'`
	}
	if p.KeepPinned {
		q += ` AND s.pinned_at = 0`
	}
	if p.KeepPolls {
		q += ` AND s.poll = ''`
	}
	if p.KeepMedia {
		q += ` AND NOT EXISTS(SELECT 1 FROM attachments a WHERE a.status_id=s.id)`
	}
	if p.KeepSelfFav {
		q += ` AND NOT EXISTS(SELECT 1 FROM likes l WHERE l.status_id=` + target + ` AND l.account_id=s.account_id)`
	}
	if p.KeepSelfBM {
		q += ` AND NOT EXISTS(SELECT 1 FROM bookmarks b WHERE b.status_id=` + target + ` AND b.account_id=s.account_id)`
	}
	if p.MinFavs > 0 {
		q += ` AND (s.boost_of_id != '' OR (SELECT COUNT(*) FROM likes l WHERE l.status_id=s.id) < ?)`
		args = append(args, p.MinFavs)
	}
	if p.MinBoosts > 0 {
		q += ` AND (s.boost_of_id != '' OR (SELECT COUNT(*) FROM statuses r WHERE r.boost_of_id=s.id) < ?)`
		args = append(args, p.MinBoosts)
	}
	return q, args
}

// cleanupCount says how many posts the policy would delete right now.
func (app *App) cleanupCount(accountID string, p CleanupPolicy) int {
	q, args := p.where(accountID, time.Now())
	return app.countInt(`SELECT COUNT(*) FROM statuses s WHERE `+q, args...)
}

// cleanupStatuses deletes a batch of old posts for every enabled policy.
func (app *App) cleanupStatuses(now time.Time) {
	rows, err := app.db.Query(`SELECT p.account_id FROM cleanup_policies p JOIN accounts a ON a.id=p.account_id
		WHERE p.enabled=1 AND a.domain='' AND a.suspended=0`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		me, err := app.AccountByID(id)
		if err != nil {
			continue
		}
		q, args := app.cleanupPolicy(id).where(id, now)
		srows, err := app.db.Query(`SELECT s.id, s.boost_of_id FROM statuses s WHERE `+q+` ORDER BY s.id LIMIT ?`, append(args, cleanupBatch)...)
		if err != nil {
			continue
		}
		var batch [][2]string
		for srows.Next() {
			var sid, boostOf string
			srows.Scan(&sid, &boostOf)
			batch = append(batch, [2]string{sid, boostOf})
		}
		srows.Close()
		for _, b := range batch {
			if b[1] != "" {
				orig, err := app.loadStatus(me, b[1])
				if err != nil { // hidden from the user now (a block, say) but maybe still there
					if orig, err = app.StatusByID(b[1]); err == nil {
						orig.Account, err = app.AccountByID(orig.AccountID)
					}
				}
				if err == nil {
					app.Unboost(me, orig)
				} else {
					app.purgeStatus(b[0]) // the boosted post is gone already
				}
				continue
			}
			if st, err := app.loadStatus(me, b[0]); err == nil {
				if err := app.DeleteStatus(me, st); err != nil {
					log.Printf("cleanup %s: %v", b[0], err)
				}
			}
		}
	}
}

func (app *App) handleCleanupSettings(w http.ResponseWriter, r *http.Request, p *Page) {
	num := func(k string) int { n, _ := strconv.Atoi(r.FormValue(k)); return n }
	age, _ := strconv.ParseInt(r.FormValue("min_age"), 10, 64)
	on := func(k string) bool { return r.FormValue(k) != "" }
	pol := CleanupPolicy{Enabled: on("enabled"), MinAge: age, KeepDirect: on("keep_direct"), KeepPinned: on("keep_pinned"),
		KeepPolls: on("keep_polls"), KeepMedia: on("keep_media"), KeepSelfFav: on("keep_self_fav"), KeepSelfBM: on("keep_self_bookmark"),
		MinFavs: num("min_favs"), MinBoosts: num("min_boosts")}
	app.setCleanupPolicy(p.Me.ID, pol)
	if pol.Enabled {
		setFlash(w, "Saved. Old posts are deleted a few at a time from now on.")
	} else {
		setFlash(w, "Saved. Automatic deletion is off.")
	}
	http.Redirect(w, r, "/settings#cleanup", http.StatusSeeOther)
}
