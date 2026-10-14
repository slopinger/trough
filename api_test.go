package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

type apiClient struct {
	t     *testing.T
	base  string
	token string
}

func (c *apiClient) do(method, path string, body any) (int, any, http.Header) {
	c.t.Helper()
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case url.Values:
		rd, ct = strings.NewReader(b.Encode()), "application/x-www-form-urlencoded"
	default:
		j, _ := json.Marshal(b)
		rd, ct = bytes.NewReader(j), "application/json"
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var v any
	json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v, resp.Header
}

func (c *apiClient) ok(method, path string, body any) any {
	c.t.Helper()
	code, v, _ := c.do(method, path, body)
	if code != 200 {
		c.t.Fatalf("%s %s -> %d %v", method, path, code, v)
	}
	return v
}

// oauthLogin performs the browser authorization code flow with PKCE.
func oauthLogin(t *testing.T, la *liveApp, user, pw, scopes string) string {
	t.Helper()
	pub := &apiClient{t: t, base: la.base}
	reg := pub.ok("POST", "/api/v1/apps", url.Values{"client_name": {"Test Client"}, "redirect_uris": {"testclient://callback"}, "scopes": {scopes}}).(map[string]any)
	clientID := reg["client_id"].(string)
	verifier := "a-very-long-pkce-verifier-string-that-is-random-enough-1234567890"
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme == "testclient" {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	post := func(path string, form url.Values) *http.Response {
		req, _ := http.NewRequest("POST", la.base+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", la.base)
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	post("/login", url.Values{"username": {user}, "password": {pw}}).Body.Close()
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"testclient://callback"}, "scope": {scopes},
		"state": {"xyz"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	resp, err := browser.Get(la.base + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), "Authorize Test Client") || strings.Contains(resp.Header.Get("Content-Security-Policy"), "form-action") {
		t.Fatalf("consent page wrong (CSP %q)", resp.Header.Get("Content-Security-Policy"))
	}
	csrf := strings.SplitN(strings.SplitN(string(page), `name="csrf" value="`, 2)[1], `"`, 2)[0]
	resp = post("/oauth/authorize", url.Values{"csrf": {csrf}, "query": {q.Encode()}, "approve": {"1"}})
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 302 || loc.Scheme != "testclient" || loc.Query().Get("state") != "xyz" {
		t.Fatalf("authorize redirect = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// wrong verifier fails
	if code, _, _ := pub.do("POST", "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"client_id": {clientID}, "redirect_uri": {"testclient://callback"}, "code_verifier": {"wrong"}}); code != 400 {
		t.Fatalf("bad PKCE verifier accepted: %d", code)
	}
	// the code was consumed by the failed attempt; authorize again
	resp = post("/oauth/authorize", url.Values{"csrf": {csrf}, "query": {q.Encode()}, "approve": {"1"}})
	resp.Body.Close()
	loc, _ = url.Parse(resp.Header.Get("Location"))
	tok := pub.ok("POST", "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"client_id": {clientID}, "redirect_uri": {"testclient://callback"}, "code_verifier": {verifier}}).(map[string]any)
	return tok["access_token"].(string)
}

func pngBytes() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 20, 10))
	for x := 0; x < 20; x++ {
		img.Set(x, 5, color.RGBA{255, 0, 0, 255})
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func TestMastodonAPI(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	token := oauthLogin(t, la, "bob", "password123", "read write follow")
	c := &apiClient{t: t, base: la.base, token: token}

	me := c.ok("GET", "/api/v1/accounts/verify_credentials", nil).(map[string]any)
	if me["id"] != bob.ID || me["acct"] != "bob" || me["source"] == nil {
		t.Fatalf("verify_credentials = %v", me)
	}
	inst := c.ok("GET", "/api/v2/instance", nil).(map[string]any)
	if inst["domain"] != la.host() || !strings.Contains(inst["version"].(string), "compatible; trough") {
		t.Fatalf("instance = %v", inst)
	}

	// post with a poll (JSON body, as Elk/Phanpy send it)
	st := c.ok("POST", "/api/v1/statuses", map[string]any{"status": "API post #api", "visibility": "public",
		"poll": map[string]any{"options": []string{"yes", "no"}, "expires_in": 3600}}).(map[string]any)
	if st["content"] == "" || st["poll"] == nil || st["account"].(map[string]any)["id"] != bob.ID {
		t.Fatalf("status = %v", st)
	}
	eventually(t, "API post federated", func() bool { return len(r.receivedOf("Create")) > 0 })
	// form-encoded reply with array params (Tusky style)
	reply := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"a reply"}, "in_reply_to_id": {st["id"].(string)}, "visibility": {"unlisted"}}).(map[string]any)
	if reply["in_reply_to_id"] != st["id"] || reply["visibility"] != "unlisted" {
		t.Fatalf("reply = %v", reply)
	}
	ctxv := c.ok("GET", "/api/v1/statuses/"+st["id"].(string)+"/context", nil).(map[string]any)
	if len(ctxv["descendants"].([]any)) != 1 {
		t.Fatalf("context = %v", ctxv)
	}

	// a remote post in the home timeline; favourite and reblog it
	remoteID := newID()
	la.insertStatus(&Status{ID: remoteID, URI: r.Base + "/s/home", AccountID: alice.ID, Visibility: "public", Content: "<p>from alice</p>", CreatedAt: time.Now()})
	code, home, hdr := c.do("GET", "/api/v1/timelines/home?limit=2", nil)
	if code != 200 || len(home.([]any)) != 2 || !strings.Contains(hdr.Get("Link"), `rel="next"`) {
		t.Fatalf("home = %d %v link=%q", code, home, hdr.Get("Link"))
	}
	fav := c.ok("POST", "/api/v1/statuses/"+remoteID+"/favourite", nil).(map[string]any)
	if fav["favourited"] != true || fav["favourites_count"].(float64) != 1 {
		t.Fatalf("favourite = %v", fav)
	}
	rb := c.ok("POST", "/api/v1/statuses/"+remoteID+"/reblog", nil).(map[string]any)
	if rb["reblog"] == nil || rb["reblog"].(map[string]any)["id"] != remoteID {
		t.Fatalf("reblog = %v", rb)
	}
	eventually(t, "Like + Announce delivered", func() bool { return len(r.receivedOf("Like")) == 1 && len(r.receivedOf("Announce")) == 1 })

	// relationships and follow via API
	rels := c.ok("GET", "/api/v1/accounts/relationships?id[]="+alice.ID, nil).([]any)
	if rels[0].(map[string]any)["following"] != true {
		t.Fatalf("relationships = %v", rels)
	}
	acct := c.ok("GET", "/api/v1/accounts/lookup?acct=alice@"+r.Host, nil).(map[string]any)
	if acct["id"] != alice.ID || acct["acct"] != "alice@"+r.Host {
		t.Fatalf("lookup = %v", acct)
	}
	res := c.ok("GET", "/api/v2/search?q="+url.QueryEscape("@alice@"+r.Host)+"&resolve=true", nil).(map[string]any)
	if len(res["accounts"].([]any)) != 1 {
		t.Fatalf("search = %v", res)
	}

	// media upload (multipart) and description update
	var mb bytes.Buffer
	mw := multipart.NewWriter(&mb)
	fw, _ := mw.CreateFormFile("file", "dot.png")
	fw.Write(pngBytes())
	mw.WriteField("description", "a red line")
	mw.Close()
	req, _ := http.NewRequest("POST", la.base+"/api/v2/media", &mb)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ := http.DefaultClient.Do(req)
	var media map[string]any
	json.NewDecoder(resp.Body).Decode(&media)
	resp.Body.Close()
	if resp.StatusCode != 200 || media["type"] != "image" || media["description"] != "a red line" {
		t.Fatalf("media = %d %v", resp.StatusCode, media)
	}
	c.ok("PUT", "/api/v1/media/"+media["id"].(string), url.Values{"description": {"a thin red line"}})
	withMedia := c.ok("POST", "/api/v1/statuses", url.Values{"status": {"pic"}, "media_ids[]": {media["id"].(string)}}).(map[string]any)
	atts := withMedia["media_attachments"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["description"] != "a thin red line" {
		t.Fatalf("attachments = %v", atts)
	}

	// notifications: alice likes bob's post
	r.send(obj{"@context": asContext, "id": r.Base + "/likes/api", "type": "Like", "actor": alice.URI, "object": la.mustURI(st["id"].(string))},
		r.Base+"/users/alice#main-key", sigCavage)
	var notifs []any
	eventually(t, "favourite notification via API", func() bool {
		notifs = c.ok("GET", "/api/v1/notifications?types[]=favourite", nil).([]any)
		return len(notifs) == 1
	})
	if n := notifs[0].(map[string]any); n["type"] != "favourite" || n["account"].(map[string]any)["id"] != alice.ID || n["status"] == nil {
		t.Fatalf("notification = %v", n)
	}

	// edit, then delete
	edited := c.ok("PUT", "/api/v1/statuses/"+reply["id"].(string), url.Values{"status": {"an edited reply"}}).(map[string]any)
	if edited["edited_at"] == nil || !strings.Contains(edited["content"].(string), "edited") {
		t.Fatalf("edit = %v", edited)
	}
	c.ok("DELETE", "/api/v1/statuses/"+reply["id"].(string), nil)
	if code, _, _ := c.do("GET", "/api/v1/statuses/"+reply["id"].(string), nil); code != 404 {
		t.Errorf("deleted status still there: %d", code)
	}

	// auth errors
	anon := &apiClient{t: t, base: la.base}
	if code, _, _ := anon.do("GET", "/api/v1/timelines/home", nil); code != 401 {
		t.Errorf("anonymous home timeline: %d", code)
	}
	ro := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read")}
	if code, _, _ := ro.do("POST", "/api/v1/statuses", url.Values{"status": {"x"}}); code != 403 {
		t.Errorf("read-only token could post: %d", code)
	}
	// CORS preflight for browser clients
	pre, _ := http.NewRequest("OPTIONS", la.base+"/api/v1/timelines/home", nil)
	pre.Header.Set("Origin", "https://elk.zone")
	pre.Header.Set("Access-Control-Request-Method", "GET")
	presp, _ := http.DefaultClient.Do(pre)
	presp.Body.Close()
	if presp.StatusCode != 204 || presp.Header.Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(presp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("preflight = %d %v", presp.StatusCode, presp.Header)
	}
}

func (la *liveApp) mustURI(id string) string {
	st, err := la.StatusByID(id)
	if err != nil {
		return ""
	}
	return st.URI
}
