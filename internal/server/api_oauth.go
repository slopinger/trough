package server

// Mastodon-compatible OAuth 2 (apps, authorization code with PKCE, client
// credentials) so third-party clients can sign in.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"git.freifick.net/slop/trough/internal/ulid"
)

const oobRedirect = "urn:ietf:wg:oauth:2.0:oob"

var knownScopes = map[string]bool{"read": true, "write": true, "follow": true, "push": true, "profile": true, "admin:read": true, "admin:write": true}

// ---- request parameters ----

// apiParams reads Mastodon API parameters from the query string, a JSON
// body, or a (multipart) form, including Rails-style `name[]` arrays and
// `poll[options][]` nesting.
type apiParams struct {
	r    *http.Request
	json map[string]any
}

func readParams(r *http.Request) *apiParams {
	p := &apiParams{r: r}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case ct == "application/json":
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		json.Unmarshal(b, &p.json)
	case ct == "multipart/form-data":
		r.ParseMultipartForm(32 << 20)
	default:
		r.ParseForm()
	}
	return p
}

func (p *apiParams) jsonPath(name string) (any, bool) {
	if p.json == nil {
		return nil, false
	}
	var cur any = p.json
	for _, part := range strings.Split(name, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// formKey turns "poll.options" into "poll[options]".
func formKey(name string) string {
	parts := strings.Split(name, ".")
	k := parts[0]
	for _, p := range parts[1:] {
		k += "[" + p + "]"
	}
	return k
}

func (p *apiParams) Get(name string) string {
	if v, ok := p.jsonPath(name); ok {
		switch x := v.(type) {
		case string:
			return x
		case bool:
			return strconv.FormatBool(x)
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64)
		}
		return ""
	}
	return p.r.FormValue(formKey(name))
}

func (p *apiParams) Has(name string) bool {
	if _, ok := p.jsonPath(name); ok {
		return true
	}
	k := formKey(name)
	_, inForm := p.r.Form[k]
	if p.r.MultipartForm != nil {
		if _, ok := p.r.MultipartForm.Value[k]; ok {
			return true
		}
	}
	return inForm || p.r.URL.Query().Has(k)
}

func (p *apiParams) List(name string) []string {
	if v, ok := p.jsonPath(name); ok {
		var out []string
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				switch y := e.(type) {
				case string:
					out = append(out, y)
				case float64:
					out = append(out, strconv.FormatFloat(y, 'f', -1, 64))
				}
			}
		case string:
			out = []string{x}
		}
		return out
	}
	k := formKey(name)
	vals := append(p.r.Form[k+"[]"], p.r.Form[k]...)
	if p.r.MultipartForm != nil {
		vals = append(vals, p.r.MultipartForm.Value[k+"[]"]...)
		vals = append(vals, p.r.MultipartForm.Value[k]...)
	}
	// numbered forms: poll[options][0]=a&poll[options][1]=b
	for i := 0; i < 20; i++ {
		if v := p.r.FormValue(k + "[" + strconv.Itoa(i) + "]"); v != "" {
			vals = append(vals, v)
		}
	}
	return vals
}

func (p *apiParams) Bool(name string) bool {
	switch strings.ToLower(p.Get(name)) {
	case "true", "1", "on", "yes":
		return true
	}
	return false
}

func (p *apiParams) Int(name string, def int) int {
	if n, err := strconv.Atoi(p.Get(name)); err == nil {
		return n
	}
	return def
}

// ---- responses ----

func apiJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	apiJSON(w, code, obj{"error": msg})
}

// cors lets browser-based clients (Elk, Phanpy, ...) call the API.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, Accept")
		h.Set("Access-Control-Expose-Headers", "Link, X-RateLimit-Reset, X-RateLimit-Limit, X-RateLimit-Remaining, X-Request-Id, Mastodon-Async-Refresh")
		h.Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- tokens ----

type apiCtx struct {
	Me     *Account // nil for app-only tokens
	AppID  string
	Scopes map[string]bool
}

func (c *apiCtx) can(scope string) bool {
	if c.Scopes[scope] {
		return true
	}
	// "read:statuses" is covered by "read"
	if i := strings.IndexByte(scope, ':'); i > 0 && c.Scopes[scope[:i]] {
		return true
	}
	return false
}

func parseScopes(s string) map[string]bool {
	out := map[string]bool{}
	for _, sc := range strings.Fields(strings.ReplaceAll(s, ",", " ")) {
		out[sc] = true
	}
	return out
}

func (app *App) apiAuth(r *http.Request) *apiCtx {
	h := r.Header.Get("Authorization")
	tok := ""
	if strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	} else if t := r.URL.Query().Get("access_token"); t != "" {
		tok = t // streaming websockets can't set headers
	}
	if tok == "" {
		return nil
	}
	var id, appID, scopes string
	var accID *string
	var lastUsed int64
	if app.db.QueryRow(`SELECT id, app_id, account_id, scopes, last_used FROM oauth_tokens WHERE token_hash=?`, sha256Hex(tok)).
		Scan(&id, &appID, &accID, &scopes, &lastUsed) != nil {
		return nil
	}
	c := &apiCtx{AppID: appID, Scopes: parseScopes(scopes)}
	if accID != nil {
		acc, err := app.AccountByID(*accID)
		if err != nil || acc.Suspended {
			return nil
		}
		c.Me = acc
		if now := time.Now().Unix(); now-lastUsed > 3600 {
			app.db.Exec(`UPDATE oauth_tokens SET last_used=? WHERE id=?`, now, id)
			app.db.Exec(`UPDATE users SET last_seen=? WHERE account_id=?`, now, acc.ID)
		}
	}
	return c
}

type apiHandler func(w http.ResponseWriter, r *http.Request, c *apiCtx)

// api wraps an API handler; scope "" allows anonymous access.
func (app *App) api(scope string, needUser bool, h apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := app.apiAuth(r)
		if c == nil {
			if scope != "" || needUser {
				if r.Header.Get("Authorization") != "" {
					apiError(w, http.StatusUnauthorized, "The access token is invalid")
				} else {
					apiError(w, http.StatusUnauthorized, "This method requires an authenticated user")
				}
				return
			}
			c = &apiCtx{Scopes: map[string]bool{}}
		}
		if needUser && c.Me == nil {
			apiError(w, http.StatusUnprocessableEntity, "This method requires an authenticated user")
			return
		}
		if scope != "" && !c.can(scope) {
			apiError(w, http.StatusForbidden, "This action is outside the authorized scopes")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, maxMediaSize+16<<20)
		}
		h(w, r, c)
	}
}

// ---- apps ----

type oauthApp struct {
	ID, ClientID, ClientSecret, Name, Website, Scopes string
	RedirectURIs                                      []string
}

func (app *App) appByClientID(clientID string) (*oauthApp, error) {
	a := &oauthApp{}
	var redirects string
	err := app.db.QueryRow(`SELECT id, client_id, client_secret, name, website, redirect_uris, scopes FROM oauth_apps WHERE client_id=?`, clientID).
		Scan(&a.ID, &a.ClientID, &a.ClientSecret, &a.Name, &a.Website, &redirects, &a.Scopes)
	a.RedirectURIs = strings.Fields(redirects)
	return a, err
}

func (app *App) handleCreateApp(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	p := readParams(r)
	name := truncateRunes(strings.TrimSpace(p.Get("client_name")), 100)
	var redirects []string
	for _, u := range append(p.List("redirect_uris"), strings.Fields(p.Get("redirect_uris"))...) {
		for _, x := range strings.Fields(u) {
			if !contains(redirects, x) {
				redirects = append(redirects, x)
			}
		}
	}
	if name == "" || len(redirects) == 0 {
		apiError(w, http.StatusUnprocessableEntity, "Validation failed: client_name and redirect_uris are required")
		return
	}
	for _, u := range redirects {
		pu, err := url.Parse(u)
		if u != oobRedirect && (err != nil || pu.Scheme == "" || strings.EqualFold(pu.Scheme, "javascript") || strings.EqualFold(pu.Scheme, "data")) {
			apiError(w, http.StatusUnprocessableEntity, "Validation failed: redirect URI must be an absolute URI")
			return
		}
	}
	scopes := p.Get("scopes")
	if scopes == "" {
		scopes = "read"
	}
	for sc := range parseScopes(scopes) {
		if !knownScopes[sc] && !knownScopes[strings.SplitN(sc, ":", 2)[0]] {
			apiError(w, http.StatusUnprocessableEntity, "Validation failed: unknown scope "+sc)
			return
		}
	}
	a := &oauthApp{ID: ulid.New(), ClientID: randomToken(20), ClientSecret: randomToken(32), Name: name,
		Website: truncateRunes(p.Get("website"), 300), RedirectURIs: redirects, Scopes: scopes}
	if _, err := app.db.Exec(`INSERT INTO oauth_apps (id,client_id,client_secret,name,website,redirect_uris,scopes,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		a.ID, a.ClientID, a.ClientSecret, a.Name, a.Website, strings.Join(redirects, " "), a.Scopes, time.Now().Unix()); err != nil {
		apiError(w, 500, "could not create app")
		return
	}
	apiJSON(w, 200, obj{"id": a.ID, "name": a.Name, "website": nullIfEmpty(a.Website), "scopes": strings.Fields(a.Scopes),
		"redirect_uri": strings.Join(redirects, "\n"), "redirect_uris": redirects,
		"client_id": a.ClientID, "client_secret": a.ClientSecret, "client_secret_expires_at": 0, "vapid_key": app.vapidPublic()})
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (app *App) handleVerifyApp(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var name, website, scopes string
	app.db.QueryRow(`SELECT name, website, scopes FROM oauth_apps WHERE id=?`, c.AppID).Scan(&name, &website, &scopes)
	apiJSON(w, 200, obj{"name": name, "website": nullIfEmpty(website), "scopes": strings.Fields(scopes), "vapid_key": app.vapidPublic()})
}

// ---- authorize (browser) ----

type authorizeReq struct {
	App                                   *oauthApp
	RedirectURI, Scope, State             string
	Challenge, ChallengeMethod, ResponseT string
}

func (app *App) parseAuthorize(q url.Values) (*authorizeReq, error) {
	a, err := app.appByClientID(q.Get("client_id"))
	if err != nil {
		return nil, errors.New("unknown client")
	}
	ar := &authorizeReq{App: a, RedirectURI: q.Get("redirect_uri"), Scope: q.Get("scope"), State: q.Get("state"),
		Challenge: q.Get("code_challenge"), ChallengeMethod: q.Get("code_challenge_method"), ResponseT: q.Get("response_type")}
	if ar.ResponseT != "code" {
		return nil, errors.New("unsupported response_type")
	}
	if ar.RedirectURI == "" && len(a.RedirectURIs) == 1 {
		ar.RedirectURI = a.RedirectURIs[0]
	}
	if !contains(a.RedirectURIs, ar.RedirectURI) {
		return nil, errors.New("redirect_uri doesn't match the app")
	}
	if ar.Scope == "" {
		ar.Scope = a.Scopes
	}
	allowed := parseScopes(a.Scopes)
	for sc := range parseScopes(ar.Scope) {
		base := strings.SplitN(sc, ":", 2)[0]
		if !allowed[sc] && !allowed[base] {
			return nil, errors.New("scope " + sc + " wasn't registered for this app")
		}
	}
	if ar.Challenge != "" && ar.ChallengeMethod != "S256" && ar.ChallengeMethod != "plain" && ar.ChallengeMethod != "" {
		return nil, errors.New("unsupported code_challenge_method")
	}
	return ar, nil
}

func (app *App) handleAuthorizeForm(w http.ResponseWriter, r *http.Request, p *Page) {
	ar, err := app.parseAuthorize(r.URL.Query())
	if err != nil {
		p.Title, p.Error = "Authorize", err.Error()
		app.renderStatus(w, http.StatusBadRequest, "authorize", p)
		return
	}
	p.Title = "Authorize " + ar.App.Name
	p.OAuth = ar
	// the form's redirect goes to the app's own URI (often a custom scheme like
	// tusky://), which a form-action 'self' policy would block
	w.Header().Set("Content-Security-Policy", strings.Replace(w.Header().Get("Content-Security-Policy"), "form-action 'self'; ", "", 1))
	p.Query = r.URL.RawQuery
	app.render(w, "authorize", p)
}

func (app *App) handleAuthorize(w http.ResponseWriter, r *http.Request, p *Page) {
	q, _ := url.ParseQuery(r.FormValue("query"))
	ar, err := app.parseAuthorize(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, _ := url.Parse(ar.RedirectURI)
	if r.FormValue("deny") != "" {
		if ar.RedirectURI == oobRedirect {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		v := target.Query()
		v.Set("error", "access_denied")
		if ar.State != "" {
			v.Set("state", ar.State)
		}
		target.RawQuery = v.Encode()
		http.Redirect(w, r, target.String(), http.StatusSeeOther)
		return
	}
	code := randomToken(24)
	app.db.Exec(`INSERT INTO oauth_codes (code, app_id, account_id, redirect_uri, scopes, challenge, challenge_method, expires_at) VALUES (?,?,?,?,?,?,?,?)`,
		sha256Hex(code), ar.App.ID, p.Me.ID, ar.RedirectURI, ar.Scope, ar.Challenge, ar.ChallengeMethod, time.Now().Add(10*time.Minute).Unix())
	if ar.RedirectURI == oobRedirect {
		p.Title, p.Query = "Authorization code", code
		p.OAuth = ar
		app.render(w, "authorize_code", p)
		return
	}
	v := target.Query()
	v.Set("code", code)
	if ar.State != "" {
		v.Set("state", ar.State)
	}
	target.RawQuery = v.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// ---- token ----

func (app *App) handleToken(w http.ResponseWriter, r *http.Request) {
	p := readParams(r)
	clientID, secret := p.Get("client_id"), p.Get("client_secret")
	if u, pw, ok := r.BasicAuth(); ok {
		clientID, secret = u, pw
	}
	a, err := app.appByClientID(clientID)
	if err != nil {
		apiJSON(w, 401, obj{"error": "invalid_client", "error_description": "unknown client"})
		return
	}
	secretOK := subtle.ConstantTimeCompare([]byte(secret), []byte(a.ClientSecret)) == 1
	var accountID *string
	scopes := p.Get("scope")
	switch p.Get("grant_type") {
	case "authorization_code":
		var accID, redirect, codeScopes, challenge, method string
		var exp int64
		err := app.db.QueryRow(`DELETE FROM oauth_codes WHERE code=? AND app_id=? RETURNING account_id, redirect_uri, scopes, challenge, challenge_method, expires_at`,
			sha256Hex(p.Get("code")), a.ID).Scan(&accID, &redirect, &codeScopes, &challenge, &method, &exp)
		if err != nil || time.Now().Unix() > exp {
			apiJSON(w, 400, obj{"error": "invalid_grant", "error_description": "The authorization code is invalid or expired"})
			return
		}
		if r := p.Get("redirect_uri"); r != "" && r != redirect {
			apiJSON(w, 400, obj{"error": "invalid_grant", "error_description": "redirect_uri mismatch"})
			return
		}
		if challenge != "" {
			verifier := p.Get("code_verifier")
			want := verifier
			if method == "S256" || method == "" {
				h := sha256.Sum256([]byte(verifier))
				want = base64.RawURLEncoding.EncodeToString(h[:])
			}
			if verifier == "" || subtle.ConstantTimeCompare([]byte(want), []byte(challenge)) != 1 {
				apiJSON(w, 400, obj{"error": "invalid_grant", "error_description": "PKCE verification failed"})
				return
			}
		} else if !secretOK {
			apiJSON(w, 401, obj{"error": "invalid_client", "error_description": "client authentication failed"})
			return
		}
		accountID, scopes = &accID, codeScopes
	case "client_credentials":
		if !secretOK {
			apiJSON(w, 401, obj{"error": "invalid_client", "error_description": "client authentication failed"})
			return
		}
		if scopes == "" {
			scopes = "read"
		}
	case "password":
		if !secretOK {
			apiJSON(w, 401, obj{"error": "invalid_client"})
			return
		}
		ip := clientIP(r)
		if app.logins.blocked(ip) {
			apiJSON(w, 429, obj{"error": "too_many_attempts"})
			return
		}
		acc, err := app.checkPassword(p.Get("username"), p.Get("password"))
		if err != nil {
			app.logins.hit(ip)
			apiJSON(w, 400, obj{"error": "invalid_grant", "error_description": "invalid username or password"})
			return
		}
		if acc.Suspended {
			apiJSON(w, 403, obj{"error": "invalid_grant", "error_description": "this account is suspended"})
			return
		}
		if app.twoFactorEnabled(acc.ID) {
			apiJSON(w, 400, obj{"error": "invalid_grant", "error_description": "two-factor authentication is on; log in through the browser"})
			return
		}
		accountID = &acc.ID
		if scopes == "" {
			scopes = a.Scopes
		}
	default:
		apiJSON(w, 400, obj{"error": "unsupported_grant_type"})
		return
	}
	token := randomToken(32)
	now := time.Now()
	if _, err := app.db.Exec(`INSERT INTO oauth_tokens (id, token_hash, app_id, account_id, scopes, created_at) VALUES (?,?,?,?,?,?)`,
		ulid.New(), sha256Hex(token), a.ID, accountID, scopes, now.Unix()); err != nil {
		apiJSON(w, 500, obj{"error": "server_error"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	apiJSON(w, 200, obj{"access_token": token, "token_type": "Bearer", "scope": scopes, "created_at": now.Unix()})
}

func (app *App) handleRevoke(w http.ResponseWriter, r *http.Request) {
	p := readParams(r)
	// knowing the token is enough to give it up (public PKCE clients have no secret)
	if a, err := app.appByClientID(p.Get("client_id")); err == nil {
		app.db.Exec(`DELETE FROM oauth_tokens WHERE token_hash=? AND app_id=?`, sha256Hex(p.Get("token")), a.ID)
	}
	apiJSON(w, 200, obj{})
}

func (app *App) handleOAuthMetadata(w http.ResponseWriter, r *http.Request) {
	base := app.cfg.BaseURL()
	var scopes []string
	for s := range knownScopes {
		scopes = append(scopes, s)
	}
	writeJSON(w, "application/json", obj{
		"issuer": base, "authorization_endpoint": base + "/oauth/authorize", "token_endpoint": base + "/oauth/token",
		"revocation_endpoint": base + "/oauth/revoke", "app_registration_endpoint": base + "/api/v1/apps",
		"scopes_supported": scopes, "response_types_supported": []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "client_credentials", "password"},
		"code_challenge_methods_supported":      []string{"S256", "plain"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post", "none"},
		"service_documentation":                 "https://git.freifick.net/slop/trough",
	})
}

// Connected apps (settings page).
type ConnectedApp struct {
	TokenID, Name, Website, Scopes string
	Created, LastUsed              time.Time
}

func (app *App) connectedApps(accountID string) []ConnectedApp {
	rows, err := app.db.Query(`SELECT t.id, a.name, a.website, t.scopes, t.created_at, t.last_used FROM oauth_tokens t JOIN oauth_apps a ON a.id=t.app_id
		WHERE t.account_id=? ORDER BY t.created_at DESC`, accountID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ConnectedApp
	for rows.Next() {
		var c ConnectedApp
		var created, used int64
		rows.Scan(&c.TokenID, &c.Name, &c.Website, &c.Scopes, &created, &used)
		c.Created = time.Unix(created, 0)
		if used > 0 {
			c.LastUsed = time.Unix(used, 0)
		}
		out = append(out, c)
	}
	return out
}
