package server

// Web Push (RFC 8030/8291/8292) for Mastodon API clients: apps register a
// push subscription per access token and get encrypted notification
// payloads through their push service, even while they're closed.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"

	"git.freifick.net/slop/trough/internal/ap"
	"git.freifick.net/slop/trough/internal/ulid"
)

var b64url = base64.RawURLEncoding

// ---- VAPID keys ----

func (app *App) vapidKey() *ecdsa.PrivateKey {
	app.instMu.Lock()
	defer app.instMu.Unlock()
	if app.vapid != nil {
		return app.vapid
	}
	if p := app.getSetting("vapid_private"); p != "" {
		if block, _ := pem.Decode([]byte(p)); block != nil {
			if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
				app.vapid = k
				return k
			}
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil
	}
	der, _ := x509.MarshalECPrivateKey(k)
	app.setSetting("vapid_private", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})))
	app.vapid = k
	return k
}

// vapidPublic is the uncompressed public key, base64url, as clients expect.
func (app *App) vapidPublic() string {
	k := app.vapidKey()
	if k == nil {
		return ""
	}
	pub, err := k.PublicKey.ECDH()
	if err != nil {
		return ""
	}
	return b64url.EncodeToString(pub.Bytes())
}

func (app *App) vapidJWT(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	header := b64url.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(obj{"aud": u.Scheme + "://" + u.Host, "exp": time.Now().Add(12 * time.Hour).Unix(), "sub": app.cfg.BaseURL() + "/about"})
	signing := header + "." + b64url.EncodeToString(claims)
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, app.vapidKey(), h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64url.EncodeToString(sig), nil
}

// ---- RFC 8291 encryption ----

func decodeB64Any(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// encryptPush encrypts a payload for a subscription (aes128gcm, one record).
func encryptPush(payload []byte, p256dh, auth string) ([]byte, error) {
	uaPub, err := decodeB64Any(p256dh)
	if err != nil {
		return nil, errors.New("bad p256dh key")
	}
	authSecret, err := decodeB64Any(auth)
	if err != nil || len(authSecret) < 16 {
		return nil, errors.New("bad auth secret")
	}
	curve := ecdh.P256()
	uaKey, err := curve.NewPublicKey(uaPub)
	if err != nil {
		return nil, errors.New("bad p256dh key")
	}
	asKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := asKey.ECDH(uaKey)
	if err != nil {
		return nil, err
	}
	asPub := asKey.PublicKey().Bytes()
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPub...), asPub...)
	ikm := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, authSecret, keyInfo), ikm); err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	prk := hkdf.Extract(sha256.New, ikm, salt)
	cek := make([]byte, 16)
	nonce := make([]byte, 12)
	io.ReadFull(hkdf.Expand(sha256.New, prk, []byte("Content-Encoding: aes128gcm\x00")), cek)
	io.ReadFull(hkdf.Expand(sha256.New, prk, []byte("Content-Encoding: nonce\x00")), nonce)
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, append(append([]byte{}, payload...), 0x02), nil)
	var out bytes.Buffer
	out.Write(salt)
	binary.Write(&out, binary.BigEndian, uint32(4096))
	out.WriteByte(byte(len(asPub)))
	out.Write(asPub)
	out.Write(ct)
	return out.Bytes(), nil
}

// ---- subscriptions ----

var pushAlertTypes = []string{"mention", "status", "reblog", "follow", "follow_request", "favourite", "poll", "update", "quote", "quoted_update", "admin.sign_up", "admin.report"}

type pushSub struct {
	ID, TokenID, Endpoint, P256dh, Auth, Policy string
	Alerts                                      map[string]bool
}

func (app *App) tokenIDFor(r *http.Request) string {
	h := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	var id string
	app.db.QueryRow(`SELECT id FROM oauth_tokens WHERE token_hash=?`, sha256Hex(h)).Scan(&id)
	return id
}

func (app *App) pushSubFor(tokenID string) (*pushSub, error) {
	s := &pushSub{}
	var alerts string
	err := app.db.QueryRow(`SELECT id, token_id, endpoint, p256dh, auth, alerts, policy FROM push_subscriptions WHERE token_id=?`, tokenID).
		Scan(&s.ID, &s.TokenID, &s.Endpoint, &s.P256dh, &s.Auth, &alerts, &s.Policy)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(alerts), &s.Alerts)
	return s, nil
}

func (app *App) apiPushJSON(s *pushSub) obj {
	alerts := obj{}
	for _, t := range pushAlertTypes {
		alerts[t] = s.Alerts[t]
	}
	return obj{"id": s.ID, "endpoint": s.Endpoint, "standard": true, "alerts": alerts, "policy": s.Policy, "server_key": app.vapidPublic()}
}

func readAlerts(p *apiParams, current map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range current {
		out[k] = v
	}
	for _, t := range pushAlertTypes {
		if p.Has("data.alerts." + t) {
			out[t] = p.Bool("data.alerts." + t)
		}
	}
	return out
}

func (app *App) apiPushSubscribe(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	p := readParams(r)
	endpoint := p.Get("subscription.endpoint")
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "https" && !(app.cfg.Insecure && u.Scheme == "http")) || u.Host == "" {
		apiError(w, 422, "Validation failed: endpoint must be an https URL")
		return
	}
	p256dh, auth := p.Get("subscription.keys.p256dh"), p.Get("subscription.keys.auth")
	if _, err := encryptPush([]byte("test"), p256dh, auth); err != nil {
		apiError(w, 422, "Validation failed: "+err.Error())
		return
	}
	tokenID := app.tokenIDFor(r)
	rawToken := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	policy := p.Get("data.policy")
	if policy != "followed" && policy != "follower" && policy != "none" {
		policy = "all"
	}
	alerts := readAlerts(p, map[string]bool{})
	ab, _ := json.Marshal(alerts)
	id := ulid.New()
	app.db.Exec(`DELETE FROM push_subscriptions WHERE token_id=?`, tokenID)
	if _, err := app.db.Exec(`INSERT INTO push_subscriptions (id, token_id, account_id, endpoint, p256dh, auth, access_token, alerts, policy, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, tokenID, c.Me.ID, endpoint, p256dh, auth, rawToken, string(ab), policy, time.Now().Unix()); err != nil {
		apiError(w, 500, "could not save subscription")
		return
	}
	s, _ := app.pushSubFor(tokenID)
	apiJSON(w, 200, app.apiPushJSON(s))
}

func (app *App) apiPushGet(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	s, err := app.pushSubFor(app.tokenIDFor(r))
	if err != nil {
		apiError(w, 404, "Record not found")
		return
	}
	apiJSON(w, 200, app.apiPushJSON(s))
}

func (app *App) apiPushUpdate(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	s, err := app.pushSubFor(app.tokenIDFor(r))
	if err != nil {
		apiError(w, 404, "Record not found")
		return
	}
	p := readParams(r)
	s.Alerts = readAlerts(p, s.Alerts)
	if pol := p.Get("policy"); pol == "all" || pol == "followed" || pol == "follower" || pol == "none" {
		s.Policy = pol
	}
	ab, _ := json.Marshal(s.Alerts)
	app.db.Exec(`UPDATE push_subscriptions SET alerts=?, policy=? WHERE id=?`, string(ab), s.Policy, s.ID)
	apiJSON(w, 200, app.apiPushJSON(s))
}

func (app *App) apiPushDelete(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	app.db.Exec(`DELETE FROM push_subscriptions WHERE token_id=?`, app.tokenIDFor(r))
	apiJSON(w, 200, obj{})
}

// ---- sending ----

var pushSem = make(chan struct{}, 4)

// pushNotification sends a notification to the recipient's push subscriptions.
func (app *App) pushNotification(accountID, notifID string) {
	var count int
	if app.db.QueryRow(`SELECT (SELECT COUNT(*) FROM push_subscriptions WHERE account_id=?) + (SELECT COUNT(*) FROM web_push_subscriptions WHERE account_id=?)`,
		accountID, accountID).Scan(&count) != nil || count == 0 {
		return
	}
	go func() {
		pushSem <- struct{}{}
		defer func() { <-pushSem }()
		me, err := app.AccountByID(accountID)
		if err != nil {
			return
		}
		ns, err := app.notificationsWhere(me, `n.account_id=? AND n.id=?`, []any{accountID, notifID}, "DESC", 1)
		if err != nil || len(ns) == 0 {
			return
		}
		n := ns[0]
		typ := notificationTypes[n.Type]
		rows, err := app.db.Query(`SELECT p.id, p.endpoint, p.p256dh, p.auth, p.alerts, p.policy, p.access_token FROM push_subscriptions p WHERE p.account_id=?`, accountID)
		if err != nil {
			return
		}
		type target struct{ id, endpoint, p256dh, auth, alerts, policy, token string }
		var subs []target
		for rows.Next() {
			var t target
			rows.Scan(&t.id, &t.endpoint, &t.p256dh, &t.auth, &t.alerts, &t.policy, &t.token)
			subs = append(subs, t)
		}
		rows.Close()
		rel := app.relationship(me.ID, n.From.ID)
		title, body := pushText(n)
		icon := app.absMedia(n.From.Avatar)
		if icon == "" {
			icon = app.defaultAvatar()
		}
		for _, s := range subs {
			var alerts map[string]bool
			json.Unmarshal([]byte(s.alerts), &alerts)
			if !alerts[typ] {
				continue
			}
			switch s.policy {
			case "none":
				continue
			case "followed":
				if !rel.Following {
					continue
				}
			case "follower":
				if !rel.FollowedBy {
					continue
				}
			}
			// like Mastodon, include the app's own token so multi-account apps know
			// which account this is (the payload is end-to-end encrypted)
			payload, _ := json.Marshal(obj{"access_token": s.token, "preferred_locale": "en", "notification_id": n.ID,
				"notification_type": typ, "icon": icon, "title": title, "body": body})
			if err := app.sendPush(s.endpoint, s.p256dh, s.auth, payload); err != nil {
				if errors.Is(err, errGone) {
					app.db.Exec(`DELETE FROM push_subscriptions WHERE id=?`, s.id)
				} else {
					log.Printf("push to %s: %v", ap.Host(s.endpoint), err)
				}
			}
		}
		// browsers subscribed from the web UI (/sw.js shows these)
		for _, s := range app.webPushTargets(accountID) {
			payload, _ := json.Marshal(obj{"title": title, "body": body, "icon": icon, "url": app.notificationURL(n), "tag": n.ID})
			if err := app.sendPush(s.endpoint, s.p256dh, s.auth, payload); err != nil {
				if errors.Is(err, errGone) {
					app.db.Exec(`DELETE FROM web_push_subscriptions WHERE id=?`, s.id)
				} else {
					log.Printf("web push to %s: %v", ap.Host(s.endpoint), err)
				}
			}
		}
	}()
}

func pushText(n *Notification) (string, string) {
	who := n.From.Name()
	body := ""
	if n.Status != nil {
		body = truncateRunes(plainText(n.Status.Content), 300)
		if n.Status.Spoiler != "" {
			body = "⚠️ " + n.Status.Spoiler
		}
	}
	switch n.Type {
	case "mention":
		return who + " mentioned you", body
	case "favourite":
		return who + " favourited your post", body
	case "reblog":
		return who + " boosted your post", body
	case "follow":
		return who + " followed you", "@" + n.From.Acct()
	case "follow_request":
		return who + " wants to follow you", "@" + n.From.Acct()
	case "poll":
		return "A poll has ended", body
	case "quote":
		return who + " quoted your post", body
	case "update":
		return who + " edited a post you boosted", body
	case "quoted_update":
		return who + " edited a post you quoted", body
	case "reaction":
		return who + " reacted to your post", body
	}
	return who, body
}

func (app *App) sendPush(endpoint, p256dh, auth string, payload []byte) error {
	body, err := encryptPush(payload, p256dh, auth)
	if err != nil {
		return err
	}
	jwt, err := app.vapidJWT(endpoint)
	if err != nil {
		return err
	}
	if _, err := app.checkRemoteURL(endpoint); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "172800")
	req.Header.Set("Urgency", "normal")
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+app.vapidPublic())
	resp, err := app.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return errGone // the subscription expired
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("push service answered %s", resp.Status)
	}
	return nil
}
