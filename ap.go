package main

import (
	"net/url"
	"strings"
	"time"
)

// Helpers for poking at loosely-typed ActivityStreams JSON.

type obj = map[string]any

const (
	asPublic   = "https://www.w3.org/ns/activitystreams#Public"
	apMimeType = "application/activity+json"
	ldMimeType = `application/ld+json; profile="https://www.w3.org/ns/activitystreams"`
)

func isPublicIRI(s string) bool {
	return s == asPublic || s == "as:Public" || s == "Public"
}

func str(m obj, k string) string {
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

func boolOf(m obj, k string) bool {
	b, _ := m[k].(bool)
	return b
}

// idOf returns the IRI of a value that is either a string, a Link (href) or
// an object (id).
func idOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case obj:
		if s := str(x, "id"); s != "" {
			return s
		}
		return str(x, "href")
	case []any:
		if len(x) > 0 {
			return idOf(x[0])
		}
	}
	return ""
}

func idsOf(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if s := idOf(e); s != "" {
				out = append(out, s)
			}
		}
	case nil:
	default:
		if s := idOf(x); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func objsOf(v any) []obj {
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

func objOf(v any) obj {
	o := objsOf(v)
	if len(o) > 0 {
		return o[0]
	}
	return nil
}

func typeOf(m obj) string {
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

// urlOf extracts a URL from url/href fields that may be strings, Links or
// arrays of Links (prefers text/html links).
func urlOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case obj:
		if s := str(x, "href"); s != "" {
			return s
		}
		if s := str(x, "url"); s != "" {
			return s
		}
		return urlOf(x["url"])
	case []any:
		for _, e := range x {
			if m, ok := e.(obj); ok && strings.HasPrefix(str(m, "mediaType"), "text/html") {
				return urlOf(m)
			}
		}
		for _, e := range x {
			if u := urlOf(e); u != "" {
				return u
			}
		}
	}
	return ""
}

func hostOf(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func sameHost(a, b string) bool {
	h := hostOf(a)
	return h != "" && h == hostOf(b)
}

func stripFragment(s string) string {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		return s[:i]
	}
	return s
}

func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func apTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

var apContext = []any{
	"https://www.w3.org/ns/activitystreams",
	"https://w3id.org/security/v1",
	obj{
		"manuallyApprovesFollowers": "as:manuallyApprovesFollowers",
		"sensitive":                 "as:sensitive",
		"Hashtag":                   "as:Hashtag",
		"toot":                      "http://joinmastodon.org/ns#",
		"discoverable":              "toot:discoverable",
		"attributionDomains":        obj{"@id": "toot:attributionDomains", "@type": "@id"},
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

// recipientsOf returns to+cc(+bto/bcc/audience) of an activity or object.
func recipientsOf(m obj) []string {
	var out []string
	for _, k := range []string{"to", "cc", "bto", "bcc", "audience"} {
		out = append(out, idsOf(m[k])...)
	}
	return out
}

// visibilityFrom derives Mastodon-style visibility from addressing.
func visibilityFrom(m obj, followersURI string) string {
	for _, r := range idsOf(m["to"]) {
		if isPublicIRI(r) {
			return "public"
		}
	}
	for _, r := range recipientsOf(m) {
		if isPublicIRI(r) {
			return "unlisted"
		}
	}
	for _, r := range recipientsOf(m) {
		if r != "" && (r == followersURI || strings.HasSuffix(r, "/followers")) {
			return "private"
		}
	}
	return "direct"
}
