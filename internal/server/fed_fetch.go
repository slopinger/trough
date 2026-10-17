package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"git.freifick.net/slop/trough/internal/ap"
	"git.freifick.net/slop/trough/internal/httpsig"
)

var (
	errGone      = errors.New("gone")
	errBlocked   = errors.New("domain is blocked")
	errNotPublic = errors.New("refusing to connect to a non-public address")
)

const maxJSONSize = 2 << 20

var cgnat = mustCIDR("100.64.0.0/10")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 0 || ip4[0] >= 240 || cgnat.Contains(ip4) {
			return false
		}
	}
	return true
}

// newHTTPClient returns a client that refuses to connect to loopback/private
// addresses (unless in insecure dev mode), so remote servers cannot make us
// fetch internal services.
func newHTTPClient(insecure bool) *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			if insecure {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if !isPublicIP(net.ParseIP(host)) {
				return errNotPublic
			}
			return nil
		},
	}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !insecure && req.URL.Scheme != "https" {
				return errors.New("redirect to non-https url")
			}
			return nil
		},
	}
}

// Apply the instance's domain policy to every redirect, including media
// requests. Checking only the first URL lets an allowed host redirect to a
// blocked host.
func (app *App) newRemoteClient() *http.Client {
	client := newHTTPClient(app.cfg.Insecure)
	checkRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := checkRedirect(req, via); err != nil {
			return err
		}
		_, err := app.checkRemoteURL(req.URL.String())
		return err
	}
	return client
}

func (app *App) userAgent() string {
	return fmt.Sprintf("%s/%s (+%s)", SoftwareName, SoftwareVersion, app.cfg.BaseURL())
}

func (app *App) checkRemoteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" && !(app.cfg.Insecure && u.Scheme == "http") {
		return nil, fmt.Errorf("unsupported url scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("url without host")
	}
	if app.domainBlocked(u.Hostname()) {
		return nil, errBlocked
	}
	return u, nil
}

// fetchJSON performs a signed GET (as the instance actor) and decodes JSON.
// It returns the decoded document and the final URL after redirects.
func (app *App) fetchJSON(ctx context.Context, raw string, accept string) (obj, string, error) {
	u, err := app.checkRemoteURL(raw)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	get := func(style string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("User-Agent", app.userAgent())
		// A signature covers the request path and host. Redirects need a fresh
		// signature rather than the headers net/http copies from the first GET.
		client := *app.client
		if inst := app.instanceActor(); inst != nil {
			if key, err := httpsig.ParsePrivateKey(inst.PrivateKey); err == nil {
				sign := httpsig.SignCavage
				if style == sigStyleRFC9421 {
					sign = httpsig.SignRFC9421
				}
				if err := sign(req, nil, inst.PublicKeyID, key); err != nil {
					return nil, err
				}
				checkRedirect := client.CheckRedirect
				client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
					if checkRedirect != nil {
						if err := checkRedirect(next, via); err != nil {
							return err
						}
					} else if len(via) >= 10 {
						return errors.New("too many redirects")
					}
					next.Host = next.URL.Host
					next.Header.Del("Signature-Input")
					return sign(next, nil, inst.PublicKeyID, key)
				}
			}
		}
		return client.Do(req)
	}
	style := app.sigStyle(u.Host)
	resp, err := get(style)
	if err == nil && style == sigStyleRFC9421 && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		// double-knock, as for deliveries
		resp.Body.Close()
		app.rejectedRFC9421(u.Host)
		resp, err = get(sigStyleCavage)
	}
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGone {
		return nil, "", errGone
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", errNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("GET %s: %s", raw, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONSize+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxJSONSize {
		return nil, "", errors.New("response too large")
	}
	var m obj
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", fmt.Errorf("GET %s: invalid json: %w", raw, err)
	}
	return m, resp.Request.URL.String(), nil
}

// fetchAP fetches an ActivityPub object and checks that it really lives at
// the host it claims to (its id must be on the same host we fetched it from).
func (app *App) fetchAP(ctx context.Context, iri string) (obj, error) {
	m, final, err := app.fetchJSON(ctx, iri, ap.MimeType+", "+ap.LDMimeType)
	if err != nil {
		return nil, err
	}
	id := ap.Str(m, "id")
	if id == "" {
		return nil, errors.New("object without id")
	}
	if !ap.SameHost(id, final) {
		if ap.StripFragment(id) == ap.StripFragment(iri) {
			return nil, errors.New("object id host mismatch")
		}
		// the server handed us something from elsewhere; fetch it from its origin once
		m2, final2, err := app.fetchJSON(ctx, id, ap.MimeType+", "+ap.LDMimeType)
		if err != nil {
			return nil, err
		}
		if ap.Str(m2, "id") != id || !ap.SameHost(id, final2) {
			return nil, errors.New("object id host mismatch")
		}
		return m2, nil
	}
	return m, nil
}

// ---- actors ----

var actorTypes = map[string]bool{"Person": true, "Service": true, "Application": true, "Group": true, "Organization": true}

const accountRefreshInterval = 24 * time.Hour

// getAccount returns the account for an actor IRI, fetching (or refreshing
// a stale copy of) it from the remote server as needed.
func (app *App) getAccount(ctx context.Context, iri string) (*Account, error) {
	iri = ap.StripFragment(iri)
	if iri == "" {
		return nil, errNotFound
	}
	acc, err := app.AccountByURI(iri)
	if err == nil {
		if acc.IsLocal() || time.Since(acc.FetchedAt) < accountRefreshInterval {
			return acc, nil
		}
		if fresh, err := app.fetchAccount(ctx, iri); err == nil {
			return fresh, nil
		}
		return acc, nil
	}
	if strings.HasPrefix(iri, app.cfg.BaseURL()+"/") {
		return nil, errNotFound
	}
	return app.fetchAccount(ctx, iri)
}

func (app *App) fetchAccount(ctx context.Context, iri string) (*Account, error) {
	m, err := app.fetchAP(ctx, iri)
	if err != nil {
		return nil, err
	}
	return app.upsertActor(m)
}

// upsertActor stores a remote actor document.
func (app *App) upsertActor(m obj) (*Account, error) {
	id := ap.Str(m, "id")
	if !actorTypes[ap.Type(m)] {
		return nil, fmt.Errorf("%s is not an actor (%s)", id, ap.Type(m))
	}
	if strings.HasPrefix(id, app.cfg.BaseURL()+"/") {
		return nil, errors.New("refusing to store a remote copy of a local actor")
	}
	username := ap.Str(m, "preferredUsername")
	inbox := ap.Str(m, "inbox")
	if id == "" || username == "" || inbox == "" {
		return nil, errors.New("actor is missing id, preferredUsername or inbox")
	}
	if len(username) > 200 {
		username = username[:200]
	}
	var keyID, keyPEM string
	for _, k := range ap.Objects(m["publicKey"]) {
		if owner := ap.Str(k, "owner"); owner != "" && owner != id {
			continue
		}
		keyID, keyPEM = ap.Str(k, "id"), ap.Str(k, "publicKeyPem")
		break
	}
	acc, err := app.AccountByURI(id)
	if err != nil {
		acc = &Account{URI: id, Domain: ap.Host(id)}
	}
	acc.Username = username
	acc.URL = ap.URL(m["url"])
	if acc.URL == "" {
		acc.URL = id
	}
	acc.DisplayName = truncateRunes(ap.Str(m, "name"), 200)
	acc.Note = sanitizeHTML(ap.Str(m, "summary"))
	acc.Avatar = ap.URL(ap.Object(m["icon"]))
	acc.Header = ap.URL(ap.Object(m["image"]))
	acc.Inbox = inbox
	acc.SharedInbox = ap.Str(ap.Object(m["endpoints"]), "sharedInbox")
	acc.Outbox = ap.Str(m, "outbox")
	acc.FollowersURI = ap.ID(m["followers"])
	acc.FollowingURI = ap.ID(m["following"])
	acc.PublicKeyID = keyID
	acc.PublicKey = keyPEM
	acc.ActorType = ap.Type(m)
	acc.Locked = ap.Bool(m, "manuallyApprovesFollowers")
	oldFields := acc.Fields
	acc.Fields = nil
	for _, a := range ap.Objects(m["attachment"]) {
		if ap.Type(a) == "PropertyValue" && len(acc.Fields) < 8 {
			acc.Fields = append(acc.Fields, Field{Name: truncateRunes(ap.Str(a, "name"), 100), Value: sanitizeHTML(ap.Str(a, "value"))})
		}
	}
	keepVerified(oldFields, acc.Fields)
	acc.Emojis = emojisOf(m)
	acc.FeaturedURI = ap.ID(m["featured"])
	acc.AlsoKnownAs = ap.IDs(m["alsoKnownAs"])
	acc.AttributionDomains = nil
	for _, d := range ap.IDs(m["attributionDomains"]) {
		if d, ok := normalizeDomain(d); ok && len(acc.AttributionDomains) < 10 {
			acc.AttributionDomains = append(acc.AttributionDomains, d)
		}
	}
	acc.MovedTo = ap.ID(m["movedTo"])
	acc.Discoverable = ap.Bool(m, "discoverable")
	acc.Indexable = ap.Bool(m, "indexable")
	acc.FetchedAt = time.Now()
	if err := app.saveAccount(acc); err != nil {
		return nil, err
	}
	app.followRemoteSuspension(acc, ap.Bool(m, "suspended"))
	app.verifyLinksLater(acc, oldFields, true)
	return acc, nil
}

func emojisOf(m obj) []Emoji {
	var out []Emoji
	for _, t := range ap.Objects(m["tag"]) {
		if ap.Type(t) != "Emoji" {
			continue
		}
		name := strings.Trim(ap.Str(t, "name"), ":")
		u := ap.URL(ap.Object(t["icon"]))
		if name != "" && u != "" && len(out) < 50 {
			out = append(out, Emoji{Shortcode: name, URL: u})
		}
	}
	return out
}

// accountForKey finds the actor owning keyID, fetching it if needed.
func (app *App) accountForKey(ctx context.Context, keyID string, refresh bool) (*Account, error) {
	if !refresh {
		if acc, err := app.accountWhere(`a.public_key_id=?`, keyID); err == nil {
			return acc, nil
		}
		if acc, err := app.AccountByURI(ap.StripFragment(keyID)); err == nil && acc.PublicKeyID == keyID {
			return acc, nil
		}
	}
	m, err := app.fetchAP(ctx, keyID)
	if err != nil {
		return nil, err
	}
	var actor obj
	if actorTypes[ap.Type(m)] && ap.Str(m, "inbox") != "" {
		actor = m
	} else if actorTypes[ap.Type(m)] {
		// GoToSocial serves the key URL as a stub actor without inbox etc.;
		// the full document lives at the actor id.
		id := ap.Str(m, "id")
		if id == "" || !ap.SameHost(id, keyID) {
			return nil, errors.New("key stub points to a different host")
		}
		actor, err = app.fetchAP(ctx, id)
		if err != nil {
			return nil, err
		}
	} else if owner := ap.Str(m, "owner"); owner != "" {
		// a standalone key document
		if !ap.SameHost(owner, keyID) {
			return nil, errors.New("key owner on a different host")
		}
		actor, err = app.fetchAP(ctx, owner)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("key document is neither an actor nor a key")
	}
	acc, err := app.upsertActor(actor)
	if err != nil {
		return nil, err
	}
	if acc.PublicKeyID != keyID || !ap.SameHost(acc.URI, keyID) {
		return nil, errors.New("key does not belong to the actor")
	}
	return acc, nil
}

// verifyRequest checks the HTTP signature of an incoming request and
// returns the signing actor.
func (app *App) verifyRequest(r *http.Request, body []byte) (*Account, error) {
	var p *httpsig.Params
	var ss string
	var err error
	if r.Header.Get("Signature-Input") != "" {
		p, ss, err = httpsig.ParseRFC9421(r, body, app.cfg.Scheme)
		if err != nil {
			return nil, err
		}
		if app.domainBlocked(ap.Host(p.KeyID)) {
			return nil, errBlocked
		}
	} else {
		p, err = httpsig.ParseCavage(r)
		if err != nil {
			return nil, err
		}
		if app.domainBlocked(ap.Host(p.KeyID)) {
			return nil, errBlocked
		}
		ss, err = httpsig.CheckCavage(r, p, body)
		if err != nil {
			return nil, err
		}
	}
	acc, err := app.accountForKey(r.Context(), p.KeyID, false)
	if err != nil {
		return nil, fmt.Errorf("fetching key %s: %w", p.KeyID, err)
	}
	err = httpsig.VerifyRSA(acc.PublicKey, ss, p.Signature, p.Algorithm)
	// maybe the key was rotated: refetch once, but not too often
	if err != nil && time.Since(acc.FetchedAt) > 5*time.Minute {
		if acc, err = app.accountForKey(r.Context(), p.KeyID, true); err == nil {
			err = httpsig.VerifyRSA(acc.PublicKey, ss, p.Signature, p.Algorithm)
		}
	}
	if err != nil {
		return nil, errors.New("invalid signature")
	}
	if r.Header.Get("Signature-Input") != "" {
		app.sawRFC9421(ap.Host(p.KeyID))
	}
	return acc, nil
}

// ---- webfinger ----

func (app *App) webfinger(ctx context.Context, user, domain string) (string, error) {
	if app.domainBlocked(domain) {
		return "", errBlocked
	}
	q := url.Values{"resource": {"acct:" + user + "@" + domain}}
	m, _, err := app.fetchJSON(ctx, app.cfg.Scheme+"://"+domain+"/.well-known/webfinger?"+q.Encode(), "application/jrd+json, application/json")
	if err != nil {
		return "", err
	}
	for _, l := range ap.Objects(m["links"]) {
		if ap.Str(l, "rel") == "self" {
			t := ap.Str(l, "type")
			if t == ap.MimeType || strings.HasPrefix(t, "application/ld+json") {
				if h := ap.Str(l, "href"); h != "" {
					return h, nil
				}
			}
		}
	}
	return "", errors.New("no activitypub actor in webfinger response")
}

// resolveAcct finds user@domain locally or via WebFinger.
func (app *App) resolveAcct(ctx context.Context, user, domain string) (*Account, error) {
	domain = strings.ToLower(domain)
	if domain == "" || domain == app.cfg.Host {
		return app.LocalAccount(user)
	}
	if acc, err := app.AccountByAcct(user, domain); err == nil {
		if time.Since(acc.FetchedAt) > accountRefreshInterval {
			if fresh, err := app.fetchAccount(ctx, acc.URI); err == nil {
				return fresh, nil
			}
		}
		return acc, nil
	}
	iri, err := app.webfinger(ctx, user, domain)
	if err != nil {
		return nil, err
	}
	acc, err := app.getAccount(ctx, iri)
	if err != nil {
		return nil, err
	}
	if acc.Domain != domain && strings.EqualFold(acc.Username, user) {
		// split-domain setup (handle on domain, actor on another host)
		acc.Domain = domain
		app.db.Exec(`UPDATE accounts SET domain=? WHERE id=?`, domain, acc.ID)
	}
	return acc, nil
}

func parseHandle(s string) (user, domain string, ok bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "acct:"), "@"))
	user, domain, _ = strings.Cut(s, "@")
	if user == "" || strings.ContainsAny(user, " /?#") || strings.ContainsAny(domain, " /?#@") {
		return "", "", false
	}
	return user, strings.ToLower(domain), true
}

// FetchObject does a signed GET of an ActivityPub object as the instance actor.
func (app *App) FetchObject(ctx context.Context, url string) (map[string]any, error) {
	m, _, err := app.fetchJSON(ctx, url, ap.MimeType+", "+ap.LDMimeType)
	return m, err
}
