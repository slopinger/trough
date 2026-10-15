package server

// Test harness for ActivityPub compatibility tests: runs a real trough
// instance on loopback and a fake remote server that speaks like other
// fediverse software. Signatures in both directions are produced/checked
// with an independent implementation (go-fed/httpsig, the library
// GoToSocial's signer descends from) so we don't just test our code against
// itself.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-fed/httpsig"
)

// ---- trough under test ----

type liveApp struct {
	*App
	base string
}

func startLiveApp(t *testing.T) *liveApp {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := ln.Addr().String()
	app, err := NewApp(&Config{Host: host, Scheme: "http", Insecure: true, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	app.Start(ctx)
	srv := &http.Server{Handler: app.Handler()}
	go srv.Serve(ln)
	t.Cleanup(func() {
		cancel()
		srv.Close()
		time.Sleep(50 * time.Millisecond) // let workers notice the cancellation
		app.Close()
	})
	return &liveApp{App: app, base: "http://" + host}
}

func (la *liveApp) host() string { return strings.TrimPrefix(la.base, "http://") }

// ---- fake remote server ----

type received struct {
	Path     string
	Activity obj
	Header   http.Header
	SigErr   error
}

type remote struct {
	t      *testing.T
	la     *liveApp
	srv    *httptest.Server
	Base   string
	Host   string
	key    *rsa.PrivateKey
	PubPEM string

	// RejectRFC9421 answers RFC 9421-signed POSTs with 401, like servers
	// that only know draft-cavage; rejected() counts them.
	RejectRFC9421 bool
	nRejected     int

	mu      sync.Mutex
	docs    map[string]string
	inbox   []received
	gets    []*http.Request
	counter int
}

func newRemote(t *testing.T, la *liveApp) *remote {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	_, pub := pemPair(t, key)
	r := &remote{t: t, la: la, key: key, PubPEM: pub, docs: map[string]string{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	r.Base = r.srv.URL
	r.Host = strings.TrimPrefix(r.Base, "http://")
	return r
}

func pemPair(t *testing.T, key *rsa.PrivateKey) (string, string) {
	t.Helper()
	priv, pub, err := keyToPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

// expand replaces template placeholders.
func (r *remote) expand(s string) string {
	pem, _ := json.Marshal(r.PubPEM)
	return strings.NewReplacer(
		"{{R}}", r.Base, "{{RH}}", r.Host, "{{L}}", r.la.base, "{{LH}}", r.la.host(),
		`"{{PEM}}"`, string(pem),
	).Replace(s)
}

func (r *remote) put(path, doc string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.docs[path] = r.expand(doc)
}

func (r *remote) rejected() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nRejected
}

func (r *remote) next() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counter++
	return r.counter
}

func (r *remote) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.gets = append(r.gets, req)
	r.mu.Unlock()
	if r.RejectRFC9421 && req.Header.Get("Signature-Input") != "" {
		r.mu.Lock()
		r.nRejected++
		r.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if req.Method == http.MethodPost {
		body, _ := io.ReadAll(req.Body)
		var act obj
		json.Unmarshal(body, &act)
		rec := received{Path: req.URL.Path, Activity: act, Header: req.Header.Clone(), SigErr: r.verifyIncoming(req, body)}
		r.mu.Lock()
		r.inbox = append(r.inbox, rec)
		r.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.URL.Path == "/.well-known/webfinger" {
		res := req.URL.Query().Get("resource")
		user := strings.TrimSuffix(strings.TrimPrefix(res, "acct:"), "@"+r.Host)
		r.mu.Lock()
		defer r.mu.Unlock()
		for path, doc := range r.docs {
			var m obj
			if json.Unmarshal([]byte(doc), &m) == nil && actorTypes[typeOf(m)] && str(m, "preferredUsername") == user {
				w.Header().Set("Content-Type", "application/jrd+json")
				json.NewEncoder(w).Encode(obj{"subject": res, "links": []obj{{"rel": "self", "type": apMimeType, "href": r.Base + path}}})
				return
			}
		}
		http.NotFound(w, req)
		return
	}
	r.mu.Lock()
	doc, ok := r.docs[req.URL.RequestURI()]
	if !ok {
		doc, ok = r.docs[req.URL.Path]
	}
	r.mu.Unlock()
	switch {
	case !ok:
		http.NotFound(w, req)
	case doc == "GONE":
		w.WriteHeader(http.StatusGone)
	default:
		w.Header().Set("Content-Type", apMimeType)
		io.WriteString(w, doc)
	}
}

// verifyIncoming checks a request from trough the way a third-party server
// would: go-fed verifier, key taken from trough's public actor document,
// Digest checked against the body.
func (r *remote) verifyIncoming(req *http.Request, body []byte) error {
	if req.Header.Get("Signature-Input") != "" {
		return r.verifyIncomingRFC9421(req, body)
	}
	req.Header.Set("Host", req.Host)
	v, err := httpsig.NewVerifier(req)
	if err != nil {
		return err
	}
	pub, err := r.fetchLocalKey(v.KeyId())
	if err != nil {
		return err
	}
	if err := v.Verify(pub, httpsig.RSA_SHA256); err != nil {
		return err
	}
	if body != nil {
		h := sha256.Sum256(body)
		if req.Header.Get("Digest") != "SHA-256="+base64.StdEncoding.EncodeToString(h[:]) {
			return fmt.Errorf("digest mismatch")
		}
	}
	return nil
}

// verifyIncomingRFC9421 is a deliberately separate RFC 9421 verifier (it
// shares no code with rfc9421.go): parse sig1's covered components, rebuild
// the signature base, check Content-Digest and the RSA signature.
func (r *remote) verifyIncomingRFC9421(req *http.Request, body []byte) error {
	input := req.Header.Get("Signature-Input")
	m := regexp.MustCompile(`^sig1=(\(([^)]*)\)(.*))$`).FindStringSubmatch(input)
	if m == nil {
		return fmt.Errorf("unexpected Signature-Input %q", input)
	}
	sigParams, covered, rest := m[1], strings.Fields(m[2]), m[3]
	keyID := regexp.MustCompile(`;keyid="([^"]+)"`).FindStringSubmatch(rest)
	created := regexp.MustCompile(`;created=(\d+)`).FindStringSubmatch(rest)
	if keyID == nil || created == nil || !strings.Contains(rest, `;alg="rsa-v1_5-sha256"`) {
		return fmt.Errorf("missing keyid/created/alg in %q", rest)
	}
	if c, _ := strconv.ParseInt(created[1], 10, 64); time.Since(time.Unix(c, 0)).Abs() > time.Minute {
		return fmt.Errorf("created %s is not now", created[1])
	}
	want := map[string]bool{`"@method"`: true, `"@target-uri"`: true}
	if req.Method == http.MethodPost {
		want[`"content-digest"`] = true
	}
	var lines []string
	for _, c := range covered {
		delete(want, c)
		name := strings.Trim(c, `"`)
		var v string
		switch name {
		case "@method":
			v = req.Method
		case "@target-uri":
			v = "http://" + req.Host + req.URL.RequestURI()
		case "@authority":
			v = req.Host
		default:
			if strings.HasPrefix(name, "@") {
				return fmt.Errorf("unexpected component %s", name)
			}
			v = req.Header.Get(name)
		}
		lines = append(lines, c+": "+v)
	}
	if len(want) > 0 {
		return fmt.Errorf("signature does not cover %v", want)
	}
	lines = append(lines, `"@signature-params": `+sigParams)
	h := sha256.Sum256(body)
	if req.Method == http.MethodPost && req.Header.Get("Content-Digest") != "sha-256=:"+base64.StdEncoding.EncodeToString(h[:])+":" {
		return fmt.Errorf("content-digest mismatch")
	}
	sm := regexp.MustCompile(`^sig1=:([A-Za-z0-9+/=]+):$`).FindStringSubmatch(req.Header.Get("Signature"))
	if sm == nil {
		return fmt.Errorf("unexpected Signature %q", req.Header.Get("Signature"))
	}
	sig, _ := base64.StdEncoding.DecodeString(sm[1])
	pub, err := r.fetchLocalKey(keyID[1])
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), crypto.SHA256, digest[:], sig)
}

func (r *remote) fetchLocalKey(keyID string) (crypto.PublicKey, error) {
	req, _ := http.NewRequest("GET", stripFragment(keyID), nil)
	req.Header.Set("Accept", apMimeType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var actor obj
	if err := json.NewDecoder(resp.Body).Decode(&actor); err != nil {
		return nil, err
	}
	pk := objOf(actor["publicKey"])
	if str(pk, "id") != keyID || str(pk, "owner") != str(actor, "id") {
		return nil, fmt.Errorf("key %s not found in actor document", keyID)
	}
	return parsePublicKey(str(pk, "publicKeyPem"))
}

// received activities of a type, in order.
func (r *remote) receivedOf(typ string) []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []received
	for _, rec := range r.inbox {
		if typeOf(rec.Activity) == typ {
			out = append(out, rec)
		}
	}
	return out
}

// ---- signing requests to trough ----

// Signature styles used by different implementations.
const (
	sigCavage     = "cavage"    // rsa-sha256, (request-target) host date digest (Mastodon, Misskey, WordPress)
	sigCavageCT   = "cavage-ct" // + content-type (Pixelfed, Friendica)
	sigPleroma    = "pleroma"   // (request-target) content-length date digest host (Pleroma/Akkoma)
	sigHS2019     = "hs2019"    // algorithm="hs2019", same headers (GoToSocial)
	sigCreated    = "created"   // hs2019 with (created)/(expires) instead of Date
	sigAuthHeader = "authz"     // "Authorization: Signature ..." header
	sigRFC9421    = "rfc9421"   // RFC 9421 HTTP Message Signatures (Fedify-based servers)
)

func (r *remote) sign(req *http.Request, body []byte, keyID, style string) {
	req.Header.Set("Host", req.Host)
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	var headers []string
	scheme := httpsig.Signature
	switch style {
	case sigCavage, sigHS2019:
		headers = []string{httpsig.RequestTarget, "host", "date", "digest"}
	case sigCavageCT:
		headers = []string{httpsig.RequestTarget, "host", "date", "digest", "content-type"}
	case sigPleroma:
		headers = []string{httpsig.RequestTarget, "content-length", "date", "digest", "host"}
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	case sigAuthHeader:
		headers = []string{httpsig.RequestTarget, "host", "date", "digest"}
		scheme = httpsig.Authorization
	case sigCreated:
		r.signCreated(req, body, keyID)
		return
	case sigRFC9421:
		r.signRFC9421(req, body, keyID)
		return
	default:
		r.t.Fatalf("unknown signature style %q", style)
	}
	signer, _, err := httpsig.NewSigner([]httpsig.Algorithm{httpsig.RSA_SHA256}, httpsig.DigestSha256, headers, scheme, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := signer.SignRequest(r.key, keyID, req, body); err != nil {
		r.t.Fatal(err)
	}
	if style == sigHS2019 {
		req.Header.Set("Signature", strings.Replace(req.Header.Get("Signature"), `algorithm="rsa-sha256"`, `algorithm="hs2019"`, 1))
	}
}

func (r *remote) rsaSign(s string, h crypto.Hash) string {
	var digest []byte
	if h == crypto.SHA512 {
		d := sha512.Sum512([]byte(s))
		digest = d[:]
	} else {
		d := sha256.Sum256([]byte(s))
		digest = d[:]
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, r.key, h, digest)
	if err != nil {
		r.t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func (r *remote) signCreated(req *http.Request, body []byte, keyID string) {
	req.Header.Del("Date")
	h := sha256.Sum256(body)
	req.Header.Set("Digest", "SHA-256="+base64.StdEncoding.EncodeToString(h[:]))
	created := time.Now().Unix()
	expires := created + 300
	ss := fmt.Sprintf("(request-target): %s %s\n(created): %d\n(expires): %d\nhost: %s\ndigest: %s",
		strings.ToLower(req.Method), req.URL.RequestURI(), created, expires, req.Host, req.Header.Get("Digest"))
	req.Header.Set("Signature", fmt.Sprintf(`keyId="%s",algorithm="hs2019",created=%d,expires=%d,headers="(request-target) (created) (expires) host digest",signature="%s"`,
		keyID, created, expires, r.rsaSign(ss, crypto.SHA256)))
}

func (r *remote) signRFC9421(req *http.Request, body []byte, keyID string) {
	req.Header.Del("Date")
	h := sha256.Sum256(body)
	req.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(h[:])+":")
	created := time.Now().Unix()
	params := fmt.Sprintf(`("@method" "@target-uri" "@authority" "content-digest");created=%d;keyid="%s";alg="rsa-v1_5-sha256"`, created, keyID)
	base := fmt.Sprintf("\"@method\": %s\n\"@target-uri\": %s\n\"@authority\": %s\n\"content-digest\": %s\n\"@signature-params\": %s",
		req.Method, req.URL.String(), req.Host, req.Header.Get("Content-Digest"), params)
	req.Header.Set("Signature-Input", "sig1="+params)
	req.Header.Set("Signature", "sig1=:"+r.rsaSign(base, crypto.SHA256)+":")
}

// send POSTs an activity to trough's shared inbox (or path) and returns the status.
func (r *remote) send(act obj, keyID, style string, path ...string) int {
	r.t.Helper()
	body, _ := json.Marshal(act)
	p := "/inbox"
	if len(path) > 0 {
		p = path[0]
	}
	req, _ := http.NewRequest("POST", r.la.base+p, bytes.NewReader(body))
	req.Header.Set("Content-Type", apMimeType)
	r.sign(req, body, keyID, style)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	msg, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		r.t.Logf("POST %s -> %d %s", p, resp.StatusCode, msg)
	}
	return resp.StatusCode
}

// signedGet fetches a trough URL as the remote actor (authorized fetch).
func (r *remote) signedGet(u, keyID string) (int, obj) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", apMimeType)
	req.Header.Set("Host", req.Host)
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	if keyID != "" {
		signer, _, _ := httpsig.NewSigner([]httpsig.Algorithm{httpsig.RSA_SHA256}, httpsig.DigestSha256,
			[]string{httpsig.RequestTarget, "host", "date", "accept"}, httpsig.Signature, 0)
		signer.SignRequest(r.key, keyID, req, nil)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m obj
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// ---- helpers ----

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func mustJSON(t *testing.T, s string) obj {
	t.Helper()
	var m obj
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad fixture JSON: %v\n%s", err, s)
	}
	return m
}

var asContext = "https://www.w3.org/ns/activitystreams"

func urlHost(u string) string {
	p, _ := url.Parse(u)
	return p.Host
}
