// Package ap has helpers for loosely-typed ActivityStreams JSON: reading
// properties that may be strings, objects or arrays, addressing and the
// JSON-LD context trough sends.
package ap

import (
	"net/url"
	"strings"
	"time"
)

type obj = map[string]any

// The public collection and the media types of ActivityPub documents.
const (
	Public     = "https://www.w3.org/ns/activitystreams#Public"
	MimeType   = "application/activity+json"
	LDMimeType = `application/ld+json; profile="https://www.w3.org/ns/activitystreams"`
)

// IsPublic reports whether s names the public collection, in any of its forms.
func IsPublic(s string) bool {
	return s == Public || s == "as:Public" || s == "Public"
}

// Str returns m[k] if it is a string, or the first string of an array.
func Str(m obj, k string) string {
	if m == nil {
		return ""
	}
	switch v := m[k].(type) {
	case string:
		return v
	case []any:
		// language maps or arrays: take the first string
		for _, x := range v {
			if s, ok := x.(string); ok {
				return s
			}
		}
	}
	return ""
}

// Bool returns m[k] if it is a boolean.
func Bool(m obj, k string) bool {
	b, _ := m[k].(bool)
	return b
}

// ID returns the IRI of a value that is either a string, a Link (href) or
// an object (id).
func ID(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case obj:
		if s := Str(x, "id"); s != "" {
			return s
		}
		return Str(x, "href")
	case []any:
		if len(x) > 0 {
			return ID(x[0])
		}
	}
	return ""
}

// IDs returns the ids of v, which may be one IRI or object, or an array of them.
func IDs(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if s := ID(e); s != "" {
				out = append(out, s)
			}
		}
	case nil:
	default:
		if s := ID(x); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Objects returns the objects in v (one object or an array), skipping bare IRIs.
func Objects(v any) []obj {
	switch x := v.(type) {
	case obj:
		return []obj{x}
	case []any:
		var out []obj
		for _, e := range x {
			if m, ok := e.(obj); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// Object returns v as an object, or the first object of an array.
func Object(v any) obj {
	o := Objects(v)
	if len(o) > 0 {
		return o[0]
	}
	return nil
}

// Type returns the type of m (the first one if it has several).
func Type(m obj) string {
	switch t := m["type"].(type) {
	case string:
		return t
	case []any:
		if len(t) > 0 {
			s, _ := t[0].(string)
			return s
		}
	}
	return ""
}

// URL extracts a URL from url/href fields that may be strings, Links or
// arrays of Links (prefers text/html links).
func URL(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case obj:
		if s := Str(x, "href"); s != "" {
			return s
		}
		if s := Str(x, "url"); s != "" {
			return s
		}
		return URL(x["url"])
	case []any:
		for _, e := range x {
			if m, ok := e.(obj); ok && strings.HasPrefix(Str(m, "mediaType"), "text/html") {
				return URL(m)
			}
		}
		for _, e := range x {
			if u := URL(e); u != "" {
				return u
			}
		}
	}
	return ""
}

// Host returns the lowercased host (with port) of an IRI.
func Host(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// SameHost reports whether two IRIs have the same, non-empty host.
func SameHost(a, b string) bool {
	h := Host(a)
	return h != "" && h == Host(b)
}

// StripFragment removes the #fragment of an IRI.
func StripFragment(s string) string {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		return s[:i]
	}
	return s
}

// ParseTime parses an xsd:dateTime, tolerating common variants.
func ParseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Time formats t the way ActivityPub servers expect.
func Time(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// Context is the JSON-LD @context of everything trough sends.
var Context = []any{
	"https://www.w3.org/ns/activitystreams",
	"https://w3id.org/security/v1",
	obj{
		"manuallyApprovesFollowers": "as:manuallyApprovesFollowers",
		"sensitive":                 "as:sensitive",
		"Hashtag":                   "as:Hashtag",
		"toot":                      "http://joinmastodon.org/ns#",
		"discoverable":              "toot:discoverable",
		"suspended":                 "toot:suspended",
		"attributionDomains":        obj{"@id": "toot:attributionDomains", "@type": "@id"},
		"featuredTags":              obj{"@id": "toot:featuredTags", "@type": "@id"},
		"indexable":                 "toot:indexable",
		"Emoji":                     "toot:Emoji",
		"schema":                    "http://schema.org#",
		"PropertyValue":             "schema:PropertyValue",
		"value":                     "schema:value",
		"quoteUri":                  "http://fedibird.com/ns#quoteUri",
		"quoteUrl":                  "as:quoteUrl",
		"quote":                     "https://w3id.org/fep/044f#quote",
		"misskey":                   "https://misskey-hub.net/ns#",
		"_misskey_quote":            "misskey:_misskey_quote",
		"quoteAuthorization":        obj{"@id": "https://w3id.org/fep/044f#quoteAuthorization", "@type": "@id"},
		"QuoteRequest":              "https://w3id.org/fep/044f#QuoteRequest",
		"gts":                       "https://gotosocial.org/ns#",
		"interactionPolicy":         obj{"@id": "gts:interactionPolicy", "@type": "@id"},
		"canQuote":                  obj{"@id": "gts:canQuote", "@type": "@id"},
		"automaticApproval":         obj{"@id": "gts:automaticApproval", "@type": "@id"},
		"manualApproval":            obj{"@id": "gts:manualApproval", "@type": "@id"},
		"EmojiReact":                "litepub:EmojiReact",
		"litepub":                   "http://litepub.social/ns#",
	},
}

// Recipients returns to+cc(+bto/bcc/audience) of an activity or object.
func Recipients(m obj) []string {
	var out []string
	for _, k := range []string{"to", "cc", "bto", "bcc", "audience"} {
		out = append(out, IDs(m[k])...)
	}
	return out
}

// Visibility derives Mastodon-style visibility from addressing.
func Visibility(m obj, followersURI string) string {
	for _, r := range IDs(m["to"]) {
		if IsPublic(r) {
			return "public"
		}
	}
	for _, r := range Recipients(m) {
		if IsPublic(r) {
			return "unlisted"
		}
	}
	for _, r := range Recipients(m) {
		if r != "" && (r == followersURI || strings.HasSuffix(r, "/followers")) {
			return "private"
		}
	}
	return "direct"
}
