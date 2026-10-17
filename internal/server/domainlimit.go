package server

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Limiting a whole server (Mastodon's "limit" domain block severity): every
// account from it, subdomains included, is treated as limited (limit.go)
// without changing the accounts themselves, so lifting it restores exactly
// what was there. Unlike a block, follows stay and its activities are
// still accepted.

// domainLimitedSQL is true for an account a whose server is limited.
const domainLimitedSQL = `EXISTS(SELECT 1 FROM domain_limits dl WHERE a.domain=dl.domain OR a.domain LIKE '%.'||dl.domain)`

// notLimitedSQL keeps accounts a that are limited neither themselves nor through their server.
const notLimitedSQL = `a.limited=0 AND NOT ` + domainLimitedSQL

func affected(res sql.Result) int64 {
	n, _ := res.RowsAffected()
	return n
}

// IsLimited reports whether the account is limited, itself or through its server.
func (a *Account) IsLimited() bool { return a.Limited || a.DomainLimited }

func (app *App) limitServer(domain, reason string) error {
	if domain == "" || strings.ContainsAny(domain, " /@") || domain == app.cfg.Host {
		return errors.New("invalid domain")
	}
	if app.domainBlocked(domain) {
		return errors.New(domain + " is blocked; unblock it first")
	}
	_, err := app.db.Exec(`INSERT INTO domain_limits (domain, reason, created_at) VALUES (?,?,?) ON CONFLICT(domain) DO UPDATE SET reason=excluded.reason`,
		domain, truncateRunes(reason, 500), time.Now().Unix())
	return err
}
