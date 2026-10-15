package server

import (
	"strings"
	"time"
)

// How we sign requests to a host: draft-cavage HTTP Signatures, which
// everyone accepts, or RFC 9421 for hosts that have sent us RFC 9421
// signatures themselves (falling back if they answer 401/403).
const (
	sigStyleCavage  = "cavage"
	sigStyleRFC9421 = "rfc9421"
)

// sigStyle is how we sign requests to host: RFC 9421 only once the host has
// shown it speaks it.
func (app *App) sigStyle(host string) string {
	var style string
	app.db.QueryRow(`SELECT style FROM signature_styles WHERE host=?`, strings.ToLower(host)).Scan(&style)
	if style == "" {
		return sigStyleCavage
	}
	return style
}

// sawRFC9421 records a valid RFC 9421 signature from host. A host that
// rejected ours keeps getting draft-cavage for a month before we try again.
func (app *App) sawRFC9421(host string) {
	app.db.Exec(`INSERT INTO signature_styles (host, style, updated_at) VALUES (?,?,?)
		ON CONFLICT(host) DO UPDATE SET style=excluded.style, updated_at=excluded.updated_at
		WHERE style=excluded.style OR updated_at < ?`,
		strings.ToLower(host), sigStyleRFC9421, time.Now().Unix(), time.Now().Add(-30*24*time.Hour).Unix())
}

func (app *App) rejectedRFC9421(host string) {
	app.db.Exec(`INSERT INTO signature_styles (host, style, updated_at) VALUES (?,?,?)
		ON CONFLICT(host) DO UPDATE SET style=excluded.style, updated_at=excluded.updated_at`,
		strings.ToLower(host), sigStyleCavage, time.Now().Unix())
}
