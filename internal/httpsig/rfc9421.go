// Package httpsig signs and verifies HTTP requests with draft-cavage HTTP
// Signatures and RFC 9421 HTTP Message Signatures (RSA, SHA-256).
package httpsig

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RFC 9421 HTTP Message Signatures, as sent by Fedify-based servers (Hollo,
// Ghost, ...) and newer Mastodon versions. We verify the first signature
// that carries a keyid. Deliveries are signed with RFC 9421 for hosts that
// have sent us valid RFC 9421 signatures themselves, falling back to
// draft-cavage (which everyone accepts) if they answer 401/403.

// SignRFC9421 adds Date, Content-Digest (if body != nil), Signature-Input and
// Signature headers, covering the same things Fedify's signatures cover.
func SignRFC9421(r *http.Request, body []byte, keyID string, key *rsa.PrivateKey) error {
	r.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	if r.Host == "" {
		r.Host = r.URL.Host
	}
	components := []string{"@method", "@target-uri", "@authority", "date"}
	if body != nil {
		h := sha256.Sum256(body)
		r.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(h[:])+":")
		components = append(components, "content-type", "content-digest")
	}
	quoted := make([]string, len(components))
	var b strings.Builder
	for i, c := range components {
		quoted[i] = strconv.Quote(c)
		var v string
		switch c {
		case "@method":
			v = r.Method
		case "@target-uri":
			v = r.URL.Scheme + "://" + r.Host + r.URL.RequestURI()
		case "@authority":
			v = strings.ToLower(r.Host)
		default:
			v = strings.TrimSpace(r.Header.Get(c))
		}
		fmt.Fprintf(&b, "%q: %s\n", c, v)
	}
	params := fmt.Sprintf(`(%s);created=%d;keyid=%s;alg="rsa-v1_5-sha256"`, strings.Join(quoted, " "), time.Now().Unix(), strconv.Quote(keyID))
	b.WriteString(`"@signature-params": ` + params)
	hash := sha256.Sum256([]byte(b.String()))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		return err
	}
	r.Header.Set("Signature-Input", "sig1="+params)
	r.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(sig)+":")
	return nil
}

type sfItem struct {
	name   string
	params map[string]string
}

// ParseRFC9421 returns the signature parameters and the signature base.
func ParseRFC9421(r *http.Request, body []byte, scheme string) (*Params, string, error) {
	input := strings.Join(r.Header.Values("Signature-Input"), ", ")
	sigs := strings.Join(r.Header.Values("Signature"), ", ")
	for _, member := range splitParams(input) {
		label, raw, ok := strings.Cut(strings.TrimSpace(member), "=")
		if !ok {
			continue
		}
		components, params, err := parseInnerList(raw)
		if err != nil {
			return nil, "", err
		}
		keyID := params["keyid"]
		if keyID == "" {
			continue
		}
		sig, err := sfByteSequence(sigs, label)
		if err != nil {
			return nil, "", err
		}
		p := &Params{KeyID: keyID, Algorithm: params["alg"], Signature: sig}
		if p.Algorithm == "" {
			p.Algorithm = "rsa-v1_5-sha256"
		}
		if c, err := strconv.ParseInt(params["created"], 10, 64); err == nil {
			p.Created = c
		}
		if e, err := strconv.ParseInt(params["expires"], 10, 64); err == nil {
			p.Expires = e
		}
		base, err := signatureBase(r, body, scheme, components, raw, p)
		if err != nil {
			return nil, "", err
		}
		return p, base, nil
	}
	return nil, "", errors.New("no usable RFC 9421 signature")
}

func signatureBase(r *http.Request, body []byte, scheme string, components []sfItem, rawParams string, p *Params) (string, error) {
	now := time.Now()
	if p.Created == 0 {
		return "", errors.New("signature without created parameter")
	}
	if c := time.Unix(p.Created, 0); c.Before(now.Add(-maxClockSkew)) || c.After(now.Add(maxClockSkew)) {
		return "", errors.New("signature created outside of allowed window")
	}
	if p.Expires != 0 && time.Unix(p.Expires, 0).Before(now) {
		return "", errors.New("signature expired")
	}
	has := map[string]bool{}
	var b strings.Builder
	for _, c := range components {
		if len(c.params) > 0 {
			return "", fmt.Errorf("unsupported component parameters on %q", c.name)
		}
		has[c.name] = true
		var v string
		switch c.name {
		case "@method":
			v = r.Method
		case "@target-uri":
			v = scheme + "://" + r.Host + r.RequestURI
		case "@authority":
			v = strings.ToLower(r.Host)
		case "@scheme":
			v = scheme
		case "@request-target":
			v = r.RequestURI
		case "@path":
			v, _, _ = strings.Cut(r.RequestURI, "?")
		case "@query":
			_, q, _ := strings.Cut(r.RequestURI, "?")
			v = "?" + q
		default:
			if strings.HasPrefix(c.name, "@") {
				return "", fmt.Errorf("unsupported derived component %q", c.name)
			}
			vals := r.Header.Values(c.name)
			if c.name == "host" && len(vals) == 0 {
				vals = []string{r.Host}
			}
			if len(vals) == 0 {
				return "", fmt.Errorf("signed header %q missing", c.name)
			}
			for i := range vals {
				vals[i] = strings.TrimSpace(vals[i])
			}
			v = strings.Join(vals, ", ")
		}
		fmt.Fprintf(&b, "%q: %s\n", c.name, v)
	}
	if !has["@method"] || !(has["@target-uri"] || (has["@authority"] || has["host"]) && (has["@path"] || has["@request-target"])) {
		return "", errors.New("signature does not cover method and target")
	}
	if r.Method == http.MethodPost {
		if !has["content-digest"] && !has["digest"] {
			return "", errors.New("signature does not cover the body digest")
		}
		if has["content-digest"] {
			if err := checkContentDigest(r.Header.Get("Content-Digest"), body); err != nil {
				return "", err
			}
		} else if err := checkDigest(r.Header.Get("Digest"), body); err != nil {
			return "", err
		}
	}
	b.WriteString(`"@signature-params": ` + rawParams)
	return b.String(), nil
}

// parseInnerList parses `("a" "b");k=v;k2="v"`.
func parseInnerList(s string) ([]sfItem, map[string]string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "(") {
		return nil, nil, errors.New("signature input is not an inner list")
	}
	end := strings.IndexByte(s, ')')
	if end < 0 {
		return nil, nil, errors.New("unterminated inner list")
	}
	var items []sfItem
	for _, tok := range strings.Fields(s[1:end]) {
		name, rest, _ := strings.Cut(tok, ";")
		unq, err := strconv.Unquote(name)
		if err != nil {
			return nil, nil, fmt.Errorf("bad component %s", name)
		}
		it := sfItem{name: strings.ToLower(unq)}
		if rest != "" {
			it.params = parseSFParams(";" + rest)
		}
		items = append(items, it)
	}
	return items, parseSFParams(s[end+1:]), nil
}

func parseSFParams(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		if k == "" {
			continue
		}
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		}
		out[strings.ToLower(k)] = v
	}
	return out
}

// sfByteSequence finds label=:base64: in a structured-field dictionary.
func sfByteSequence(dict, label string) ([]byte, error) {
	for _, member := range splitParams(dict) {
		k, v, ok := strings.Cut(strings.TrimSpace(member), "=")
		if !ok || k != label {
			continue
		}
		v = strings.TrimSpace(v)
		if i := strings.IndexByte(v[1:], ':'); strings.HasPrefix(v, ":") && i >= 0 {
			return base64.StdEncoding.DecodeString(v[1 : i+1])
		}
		return nil, errors.New("signature is not a byte sequence")
	}
	return nil, fmt.Errorf("no signature labelled %q", label)
}

func checkContentDigest(header string, body []byte) error {
	if header == "" {
		return errors.New("missing content-digest")
	}
	for _, member := range splitParams(header) {
		alg, v, ok := strings.Cut(strings.TrimSpace(member), "=")
		if !ok {
			continue
		}
		want := strings.Trim(strings.TrimSpace(v), ":")
		var got string
		switch strings.ToLower(alg) {
		case "sha-256":
			h := sha256.Sum256(body)
			got = base64.StdEncoding.EncodeToString(h[:])
		case "sha-512":
			h := sha512.Sum512(body)
			got = base64.StdEncoding.EncodeToString(h[:])
		default:
			continue
		}
		if got != want {
			return errors.New("content-digest mismatch")
		}
		return nil
	}
	return errors.New("no supported content-digest algorithm")
}
