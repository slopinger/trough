package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTP Signatures (draft-cavage-http-signatures-12), the flavour spoken by
// Mastodon, GoToSocial, Misskey, Pleroma, ...

func generateKey() (privPEM, pubPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	return keyToPEM(key)
}

func keyToPEM(key *rsa.PrivateKey) (privPEM, pubPEM string, err error) {
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	privPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
	pubPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return
}

func parsePrivateKey(s string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("no PEM private key")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errors.New("not an RSA key")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func parsePublicKey(s string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(s)))
	if block == nil {
		return nil, errors.New("no PEM public key")
	}
	if k, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PublicKey); ok {
			return rk, nil
		}
		return nil, errors.New("unsupported public key type")
	}
	return x509.ParsePKCS1PublicKey(block.Bytes)
}

func digestHeader(body []byte) string {
	h := sha256.Sum256(body)
	return "SHA-256=" + base64.StdEncoding.EncodeToString(h[:])
}

func requestTarget(r *http.Request) string {
	uri := r.RequestURI // incoming requests: the raw request line, exactly as signed
	if uri == "" {
		uri = r.URL.RequestURI()
	}
	return strings.ToLower(r.Method) + " " + uri
}

// signRequest adds Date, Digest (if body != nil) and Signature headers.
func signRequest(r *http.Request, body []byte, keyID string, key *rsa.PrivateKey) error {
	r.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	if r.Host == "" {
		r.Host = r.URL.Host
	}
	headers := []string{"(request-target)", "host", "date"}
	if body != nil {
		r.Header.Set("Digest", digestHeader(body))
		headers = append(headers, "digest")
	}
	var sb strings.Builder
	for i, h := range headers {
		if i > 0 {
			sb.WriteByte('\n')
		}
		switch h {
		case "(request-target)":
			sb.WriteString(h + ": " + requestTarget(r))
		case "host":
			sb.WriteString("host: " + r.Host)
		default:
			sb.WriteString(h + ": " + r.Header.Get(h))
		}
	}
	hash := sha256.Sum256([]byte(sb.String()))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		return err
	}
	r.Header.Set("Signature", fmt.Sprintf(`keyId="%s",algorithm="rsa-sha256",headers="%s",signature="%s"`,
		keyID, strings.Join(headers, " "), base64.StdEncoding.EncodeToString(sig)))
	return nil
}

type sigParams struct {
	KeyID, Algorithm string
	Headers          []string
	Signature        []byte
	Created, Expires int64
}

func parseSignatureHeader(r *http.Request) (*sigParams, error) {
	h := r.Header.Get("Signature")
	if h == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Signature ") {
			h = strings.TrimPrefix(auth, "Signature ")
		}
	}
	if h == "" {
		return nil, errors.New("no signature")
	}
	p := &sigParams{Headers: []string{"date"}}
	for _, part := range splitParams(h) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch k {
		case "keyid":
			p.KeyID = v
		case "algorithm":
			p.Algorithm = strings.ToLower(v)
		case "headers":
			p.Headers = strings.Fields(strings.ToLower(v))
		case "signature":
			sig, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return nil, errors.New("bad signature encoding")
			}
			p.Signature = sig
		case "created":
			p.Created, _ = strconv.ParseInt(v, 10, 64)
		case "expires":
			p.Expires, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if p.KeyID == "" || len(p.Signature) == 0 {
		return nil, errors.New("incomplete signature")
	}
	return p, nil
}

// splitParams splits a,b="x,y",c on commas outside quotes.
func splitParams(s string) []string {
	var out []string
	inQ := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQ = !inQ
		case ',':
			if !inQ {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

const maxClockSkew = time.Hour

// checkSignatureShape validates everything except the cryptographic part:
// that the important headers are covered, the date is fresh and the digest
// matches the body. It returns the signing string.
func checkSignatureShape(r *http.Request, p *sigParams, body []byte) (string, error) {
	has := map[string]bool{}
	for _, h := range p.Headers {
		has[h] = true
	}
	if !has["(request-target)"] {
		return "", errors.New("signature does not cover (request-target)")
	}
	if !has["host"] {
		return "", errors.New("signature does not cover host")
	}
	if !has["date"] && !has["(created)"] {
		return "", errors.New("signature does not cover date")
	}
	if r.Method == http.MethodPost && !has["digest"] {
		return "", errors.New("signature does not cover digest")
	}
	now := time.Now()
	if has["date"] {
		d, err := http.ParseTime(r.Header.Get("Date"))
		if err != nil {
			return "", errors.New("bad date header")
		}
		if d.Before(now.Add(-maxClockSkew)) || d.After(now.Add(maxClockSkew)) {
			return "", errors.New("date outside of allowed window")
		}
	}
	if p.Expires != 0 && time.Unix(p.Expires, 0).Before(now) {
		return "", errors.New("signature expired")
	}
	if has["digest"] {
		if err := checkDigest(r.Header.Get("Digest"), body); err != nil {
			return "", err
		}
	}
	var sb strings.Builder
	for i, h := range p.Headers {
		if i > 0 {
			sb.WriteByte('\n')
		}
		switch h {
		case "(request-target)":
			sb.WriteString(h + ": " + requestTarget(r))
		case "(created)":
			sb.WriteString(h + ": " + strconv.FormatInt(p.Created, 10))
		case "(expires)":
			sb.WriteString(h + ": " + strconv.FormatInt(p.Expires, 10))
		case "host":
			sb.WriteString("host: " + r.Host)
		default:
			vals := r.Header.Values(h)
			if len(vals) == 0 {
				return "", fmt.Errorf("signed header %q missing", h)
			}
			sb.WriteString(h + ": " + strings.Join(vals, ", "))
		}
	}
	return sb.String(), nil
}

func checkDigest(header string, body []byte) error {
	if header == "" {
		return errors.New("missing digest")
	}
	for _, part := range strings.Split(header, ",") {
		alg, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(alg) {
		case "SHA-256":
			h := sha256.Sum256(body)
			if base64.StdEncoding.EncodeToString(h[:]) == val {
				return nil
			}
			return errors.New("digest mismatch")
		case "SHA-512":
			h := sha512.Sum512(body)
			if base64.StdEncoding.EncodeToString(h[:]) == val {
				return nil
			}
			return errors.New("digest mismatch")
		}
	}
	return errors.New("no supported digest algorithm")
}

func verifyRSA(pubPEM, signingString string, sig []byte, alg string) error {
	pub, err := parsePublicKey(pubPEM)
	if err != nil {
		return err
	}
	switch alg {
	case "", "rsa-sha256", "hs2019", "rsa-v1_5-sha256":
	case "rsa-sha512":
		h := sha512.Sum512([]byte(signingString))
		return rsa.VerifyPKCS1v15(pub, crypto.SHA512, h[:], sig)
	case "rsa-pss-sha512":
		h := sha512.Sum512([]byte(signingString))
		return rsa.VerifyPSS(pub, crypto.SHA512, h[:], sig, &rsa.PSSOptions{SaltLength: 64})
	default:
		return fmt.Errorf("unsupported algorithm %q", alg)
	}
	h := sha256.Sum256([]byte(signingString))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig)
}
