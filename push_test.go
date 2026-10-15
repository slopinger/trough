package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/hkdf"
)

// decryptPush is the receiving side of RFC 8291 (what a browser/app does).
func decryptPush(t *testing.T, body []byte, ua *ecdh.PrivateKey, auth []byte) []byte {
	t.Helper()
	salt, idlen := body[:16], int(body[20])
	asPub, ct := body[21:21+idlen], body[21+idlen:]
	as, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := ua.ECDH(as)
	info := append(append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...), asPub...)
	ikm := make([]byte, 32)
	io.ReadFull(hkdf.New(sha256.New, shared, auth, info), ikm)
	prk := hkdf.Extract(sha256.New, ikm, salt)
	cek, nonce := make([]byte, 16), make([]byte, 12)
	io.ReadFull(hkdf.Expand(sha256.New, prk, []byte("Content-Encoding: aes128gcm\x00")), cek)
	io.ReadFull(hkdf.Expand(sha256.New, prk, []byte("Content-Encoding: nonce\x00")), nonce)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	return bytes.TrimRight(bytes.TrimSuffix(bytes.TrimRight(pt, "\x00"), []byte{2}), "\x00")
}

func verifyVAPID(t *testing.T, header, serverKey string) {
	t.Helper()
	var jwt, k string
	for _, part := range strings.Split(strings.TrimPrefix(header, "vapid "), ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if kv[0] == "t" {
			jwt = kv[1]
		} else if kv[0] == "k" {
			k = kv[1]
		}
	}
	if k != serverKey {
		t.Fatalf("VAPID k = %q, want server key", k)
	}
	parts := strings.Split(jwt, ".")
	pub, _ := b64url.DecodeString(k)
	x, y := elliptic.Unmarshal(elliptic.P256(), pub)
	sig, _ := b64url.DecodeString(parts[2])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("VAPID JWT signature invalid")
	}
}

func TestWebPush(t *testing.T) {
	la, r, bob, alice := featureFixture(t)
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	var mu sync.Mutex
	var got [][]byte
	var vapid []string
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		if req.Header.Get("Content-Encoding") != "aes128gcm" {
			http.Error(w, "bad encoding", 400)
			return
		}
		mu.Lock()
		got = append(got, b)
		vapid = append(vapid, req.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(201)
	}))
	defer svc.Close()

	token := oauthLogin(t, la, "bob", "password123", "read write push")
	c := &apiClient{t: t, base: la.base, token: token}
	inst := c.ok("GET", "/api/v2/instance", nil).(map[string]any)
	serverKey := inst["configuration"].(map[string]any)["vapid"].(map[string]any)["public_key"].(string)
	sub := c.ok("POST", "/api/v1/push/subscription", map[string]any{
		"subscription": map[string]any{"endpoint": svc.URL + "/push/abc", "keys": map[string]any{
			"p256dh": b64url.EncodeToString(ua.PublicKey().Bytes()), "auth": b64url.EncodeToString(auth)}},
		"data": map[string]any{"alerts": map[string]any{"favourite": true, "mention": true}, "policy": "all"},
	}).(map[string]any)
	if sub["server_key"] != serverKey || sub["alerts"].(map[string]any)["favourite"] != true || sub["alerts"].(map[string]any)["follow"] != false {
		t.Fatalf("subscription = %v", sub)
	}
	mine, _ := la.PostStatus(t.Context(), bob, PostInput{Text: "push me", Visibility: "public"})
	r.send(obj{"@context": asContext, "id": r.Base + "/likes/push", "type": "Like", "actor": alice.URI, "object": mine.URI}, r.Base+"/users/alice#main-key", sigCavage)
	eventually(t, "push delivered", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 })
	var payload map[string]any
	if err := json.Unmarshal(decryptPush(t, got[0], ua, auth), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["notification_type"] != "favourite" || payload["access_token"] != token || !strings.Contains(payload["title"].(string), "favourited") {
		t.Fatalf("payload = %v", payload)
	}
	verifyVAPID(t, vapid[0], serverKey)
	// alerts that are off don't push
	c.ok("PUT", "/api/v1/push/subscription", map[string]any{"data": map[string]any{"alerts": map[string]any{"favourite": false}}})
	r.send(obj{"@context": asContext, "id": r.Base + "/likes/push2", "type": "Like", "actor": alice.URI, "object": la.mustURI(mine.ID)}, r.Base+"/users/alice#main-key", sigCavage)
	la.Follow(alice, bob) // also not subscribed (follow alert off)
	if s := c.ok("GET", "/api/v1/push/subscription", nil).(map[string]any); s["alerts"].(map[string]any)["favourite"] != false {
		t.Fatalf("update didn't stick: %v", s)
	}
	c.ok("DELETE", "/api/v1/push/subscription", nil)
	if code, _, _ := c.do("GET", "/api/v1/push/subscription", nil); code != 404 {
		t.Errorf("deleted subscription still there: %d", code)
	}
	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n != 1 {
		t.Errorf("got %d pushes, want 1", n)
	}
	// tokens without the push scope can't subscribe
	ro := &apiClient{t: t, base: la.base, token: oauthLogin(t, la, "bob", "password123", "read")}
	if code, _, _ := ro.do("GET", "/api/v1/push/subscription", nil); code != 403 {
		t.Errorf("push without scope: %d", code)
	}
}
