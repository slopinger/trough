package server

// ActivityPub compatibility tests. Each "flavor" reproduces the actor and
// object shapes and the HTTP signature style of a real implementation; the
// shared scenario then runs a whole relationship lifecycle against a live
// trough instance.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.freifick.net/slop/trough/internal/ap"
)

type flavor struct {
	name      string
	style     string            // signature style of outgoing requests
	actorPath string            // path of the actor document
	actor     string            // actor JSON template
	keyID     string            // keyId template
	docs      map[string]string // extra documents (key stubs, ...)
	notePath  string            // path prefix of post ids
	note      string            // post JSON template ({{ID}}, {{CONTENT}})
	// optional quirks
	tweak     func(act obj)  // adjust outgoing activities
	likeExtra map[string]any // extra Like members (Misskey reactions)
	wantAtt   string         // expected attachment media type ("" = none)
	wantEmoji string         // expected custom emoji shortcode on the post
	wantTag   string         // expected hashtag
}

// ---- actor templates ----

const mastodonActor = `{
 "@context": ["https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1", {
   "manuallyApprovesFollowers": "as:manuallyApprovesFollowers", "toot": "http://joinmastodon.org/ns#",
   "featured": {"@id": "toot:featured", "@type": "@id"}, "featuredTags": {"@id": "toot:featuredTags", "@type": "@id"},
   "alsoKnownAs": {"@id": "as:alsoKnownAs", "@type": "@id"}, "movedTo": {"@id": "as:movedTo", "@type": "@id"},
   "schema": "http://schema.org#", "PropertyValue": "schema:PropertyValue", "value": "schema:value",
   "discoverable": "toot:discoverable", "suspended": "toot:suspended", "memorial": "toot:memorial",
   "indexable": "toot:indexable", "Emoji": "toot:Emoji", "focalPoint": {"@container": "@list", "@id": "toot:focalPoint"}}],
 "id": "{{R}}/users/alice", "type": "Person",
 "following": "{{R}}/users/alice/following", "followers": "{{R}}/users/alice/followers",
 "inbox": "{{R}}/users/alice/inbox", "outbox": "{{R}}/users/alice/outbox",
 "featured": "{{R}}/users/alice/collections/featured", "featuredTags": "{{R}}/users/alice/collections/tags",
 "preferredUsername": "alice", "name": "Alice :blobcat:", "summary": "<p>Mastodon user</p>",
 "url": "{{R}}/@alice", "manuallyApprovesFollowers": false, "discoverable": true, "indexable": true,
 "published": "2023-04-01T00:00:00Z", "memorial": false, "devices": "{{R}}/users/alice/collections/devices",
 "alsoKnownAs": [],
 "publicKey": {"id": "{{R}}/users/alice#main-key", "owner": "{{R}}/users/alice", "publicKeyPem": "{{PEM}}"},
 "tag": [{"id": "{{R}}/emojis/1", "type": "Emoji", "name": ":blobcat:", "updated": "2023-01-01T00:00:00Z",
   "icon": {"type": "Image", "mediaType": "image/png", "url": "{{R}}/system/emoji/blobcat.png"}}],
 "attachment": [{"type": "PropertyValue", "name": "Web", "value": "<a href=\"https://example.com\" rel=\"me\">example.com</a>"}],
 "endpoints": {"sharedInbox": "{{R}}/inbox"},
 "icon": {"type": "Image", "mediaType": "image/png", "url": "{{R}}/system/avatar.png"},
 "image": {"type": "Image", "mediaType": "image/png", "url": "{{R}}/system/header.png"}
}`

const mastodonNote = `{
 "id": "{{ID}}", "type": "Note", "summary": null, "inReplyTo": null,
 "published": "2026-01-01T12:00:00Z", "url": "{{R}}/@alice/1", "attributedTo": "{{R}}/users/alice",
 "to": ["https://www.w3.org/ns/activitystreams#Public"], "cc": ["{{R}}/users/alice/followers", "{{L}}/users/bob"],
 "sensitive": false, "atomUri": "{{ID}}", "inReplyToAtomUri": null,
 "conversation": "tag:{{RH}},2026-01-01:objectId=1:objectType=Conversation",
 "content": "<p>{{CONTENT}} <span class=\"h-card\" translate=\"no\"><a href=\"{{L}}/@bob\" class=\"u-url mention\">@<span>bob</span></a></span> <a href=\"{{R}}/tags/trough\" class=\"mention hashtag\" rel=\"tag\">#<span>trough</span></a> :blobcat:</p>",
 "contentMap": {"en": "<p>{{CONTENT}}</p>"},
 "attachment": [{"type": "Document", "mediaType": "image/png", "url": "{{R}}/system/media/1.png", "name": "a cat",
   "blurhash": "UEHLh[WB2yk8pyoJadR*.7kCMdnjS#M|%1%2", "focalPoint": [0.0, 0.0], "width": 640, "height": 480}],
 "tag": [{"type": "Mention", "href": "{{L}}/users/bob", "name": "@bob@{{LH}}"},
   {"type": "Hashtag", "href": "{{R}}/tags/trough", "name": "#trough"},
   {"id": "{{R}}/emojis/1", "type": "Emoji", "name": ":blobcat:", "updated": "2023-01-01T00:00:00Z",
    "icon": {"type": "Image", "mediaType": "image/png", "url": "{{R}}/system/emoji/blobcat.png"}}],
 "replies": {"id": "{{ID}}/replies", "type": "Collection",
   "first": {"type": "CollectionPage", "next": "{{ID}}/replies?only_other_accounts=true&page=true", "partOf": "{{ID}}/replies", "items": []}},
 "likes": {"id": "{{ID}}/likes", "type": "Collection", "totalItems": 0},
 "shares": {"id": "{{ID}}/shares", "type": "Collection", "totalItems": 0}
}`

// GoToSocial: the key lives at its own URL that returns a stub actor
// without inbox; single values are not wrapped in arrays.
const gtsActor = `{
 "@context": ["https://w3id.org/security/v1", "https://www.w3.org/ns/activitystreams",
   {"discoverable": "http://joinmastodon.org/ns#discoverable", "manuallyApprovesFollowers": "http://www.w3.org/ns/activitystreams#manuallyApprovesFollowers", "toot": "http://joinmastodon.org/ns#"}],
 "discoverable": true, "endpoints": {"sharedInbox": "{{R}}/sharedInbox"},
 "featured": "{{R}}/users/alice/collections/featured", "followers": "{{R}}/users/alice/followers",
 "following": "{{R}}/users/alice/following", "id": "{{R}}/users/alice", "inbox": "{{R}}/users/alice/inbox",
 "manuallyApprovesFollowers": false, "name": "Alice", "outbox": "{{R}}/users/alice/outbox",
 "preferredUsername": "alice", "published": "2023-01-01T00:00:00Z",
 "publicKey": {"id": "{{R}}/users/alice/main-key", "owner": "{{R}}/users/alice", "publicKeyPem": "{{PEM}}"},
 "summary": "<p>GoToSocial user</p>", "tag": [], "type": "Person", "url": "{{R}}/@alice",
 "icon": {"mediaType": "image/webp", "type": "Image", "url": "{{R}}/fileserver/avatar.webp"}
}`

const gtsKeyStub = `{
 "@context": ["https://w3id.org/security/v1", "https://www.w3.org/ns/activitystreams"],
 "id": "{{R}}/users/alice", "preferredUsername": "alice", "type": "Person",
 "publicKey": {"id": "{{R}}/users/alice/main-key", "owner": "{{R}}/users/alice", "publicKeyPem": "{{PEM}}"}
}`

const gtsNote = `{
 "@context": "https://www.w3.org/ns/activitystreams",
 "attachment": {"mediaType": "image/jpeg", "name": "a photo", "type": "Document", "url": "{{R}}/fileserver/1.jpg", "blurhash": "LNJ@D"},
 "attributedTo": "{{R}}/users/alice", "cc": "{{R}}/users/alice/followers",
 "content": "<p>{{CONTENT}} <span class=\"h-card\"><a href=\"{{L}}/@bob\" class=\"u-url mention\" rel=\"nofollow noreferrer noopener\" target=\"_blank\">@<span>bob</span></a></span> <a href=\"{{R}}/tags/trough\" class=\"mention hashtag\" rel=\"tag nofollow noreferrer noopener\" target=\"_blank\">#<span>trough</span></a></p>",
 "contentMap": {"en": "<p>{{CONTENT}}</p>"}, "id": "{{ID}}", "published": "2026-01-01T12:00:00Z",
 "interactionPolicy": {"canLike": {"always": ["https://www.w3.org/ns/activitystreams#Public"], "approvalRequired": []},
   "canReply": {"always": ["https://www.w3.org/ns/activitystreams#Public"], "approvalRequired": []},
   "canAnnounce": {"always": ["https://www.w3.org/ns/activitystreams#Public"], "approvalRequired": []}},
 "replies": {"first": {"id": "{{ID}}/replies?page=true", "next": "{{ID}}/replies?only_other_accounts=false&page=true", "partOf": "{{ID}}/replies", "type": "CollectionPage"}, "id": "{{ID}}/replies", "type": "Collection"},
 "sensitive": false, "summary": "",
 "tag": [{"href": "{{L}}/users/bob", "name": "@bob@{{LH}}", "type": "Mention"}, {"href": "{{R}}/tags/trough", "name": "#trough", "type": "Hashtag"}],
 "to": ["https://www.w3.org/ns/activitystreams#Public", "{{L}}/users/bob"], "type": "Note", "url": "{{R}}/@alice/statuses/1"
}`

// Misskey/Sharkey/Firefish: opaque ids, _misskey_* extensions, reactions
// sent as Like with content.
const misskeyActor = `{
 "@context": ["https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1", {"Key": "sec:Key", "manuallyApprovesFollowers": "as:manuallyApprovesFollowers", "sensitive": "as:sensitive", "Hashtag": "as:Hashtag", "quoteUrl": "as:quoteUrl", "toot": "http://joinmastodon.org/ns#", "Emoji": "toot:Emoji", "featured": "toot:featured", "discoverable": "toot:discoverable", "schema": "http://schema.org#", "PropertyValue": "schema:PropertyValue", "value": "schema:value", "misskey": "https://misskey-hub.net/ns#", "_misskey_content": "misskey:_misskey_content", "_misskey_quote": "misskey:_misskey_quote", "_misskey_reaction": "misskey:_misskey_reaction", "_misskey_votes": "misskey:_misskey_votes", "_misskey_summary": "misskey:_misskey_summary", "isCat": "misskey:isCat", "vcard": "http://www.w3.org/2006/vcard/ns#"}],
 "type": "Person", "id": "{{R}}/users/9h3k2l1abc", "inbox": "{{R}}/users/9h3k2l1abc/inbox", "outbox": "{{R}}/users/9h3k2l1abc/outbox",
 "followers": "{{R}}/users/9h3k2l1abc/followers", "following": "{{R}}/users/9h3k2l1abc/following",
 "featured": "{{R}}/users/9h3k2l1abc/collections/featured", "sharedInbox": "{{R}}/inbox", "endpoints": {"sharedInbox": "{{R}}/inbox"},
 "url": "{{R}}/@alice", "preferredUsername": "alice", "name": "Alice :misskey:", "summary": "<p><span>Misskey user</span></p>",
 "_misskey_summary": "Misskey user", "icon": {"type": "Image", "url": "{{R}}/files/avatar.webp", "sensitive": false, "name": null},
 "image": null, "tag": [{"id": "{{R}}/emojis/misskey", "type": "Emoji", "name": ":misskey:", "updated": "2023-01-01T00:00:00.000Z", "icon": {"type": "Image", "mediaType": "image/png", "url": "{{R}}/files/misskey.png"}}],
 "manuallyApprovesFollowers": false, "discoverable": true,
 "publicKey": {"id": "{{R}}/users/9h3k2l1abc#main-key", "type": "Key", "owner": "{{R}}/users/9h3k2l1abc", "publicKeyPem": "{{PEM}}"},
 "isCat": true, "vcard:bday": "2000-01-01", "attachment": [{"type": "PropertyValue", "name": "pronouns", "value": "she/her"}]
}`

const misskeyNote = `{
 "id": "{{ID}}", "type": "Note", "attributedTo": "{{R}}/users/9h3k2l1abc",
 "content": "<p><span>{{CONTENT}} </span><a href=\"{{L}}/@bob\" class=\"u-url mention\">@bob@{{LH}}</a><span> </span><a href=\"{{R}}/tags/trough\" rel=\"tag\">#trough</a><span> :misskey:</span></p>",
 "_misskey_content": "{{CONTENT}} @bob@{{LH}} #trough :misskey:",
 "source": {"content": "{{CONTENT}} @bob@{{LH}} #trough :misskey:", "mediaType": "text/x.misskey.markdown"},
 "published": "2026-01-01T12:00:00.000Z", "to": ["https://www.w3.org/ns/activitystreams#Public"],
 "cc": ["{{R}}/users/9h3k2l1abc/followers", "{{L}}/users/bob"], "inReplyTo": null,
 "attachment": [{"type": "Document", "mediaType": "image/webp", "url": "{{R}}/files/pic.webp", "name": null, "sensitive": false}],
 "sensitive": false,
 "tag": [{"type": "Mention", "href": "{{L}}/users/bob", "name": "@bob@{{LH}}"}, {"type": "Hashtag", "href": "{{R}}/tags/trough", "name": "#trough"},
   {"id": "{{R}}/emojis/misskey", "type": "Emoji", "name": ":misskey:", "updated": "2023-01-01T00:00:00.000Z", "icon": {"type": "Image", "mediaType": "image/png", "url": "{{R}}/files/misskey.png"}}]
}`

// Pleroma/Akkoma: Note carries "actor", "context"; content-length is signed.
const pleromaActor = `{
 "@context": ["https://www.w3.org/ns/activitystreams", "{{R}}/schemas/litepub-0.1.jsonld", {"@language": "und"}],
 "alsoKnownAs": [], "attachment": [], "capabilities": {"acceptsChatMessages": true},
 "discoverable": true, "endpoints": {"oauthAuthorizationEndpoint": "{{R}}/oauth/authorize", "oauthRegistrationEndpoint": "{{R}}/api/v1/apps",
   "oauthTokenEndpoint": "{{R}}/oauth/token", "sharedInbox": "{{R}}/inbox", "uploadMedia": "{{R}}/api/ap/upload_media"},
 "featured": "{{R}}/users/alice/collections/featured", "followers": "{{R}}/users/alice/followers", "following": "{{R}}/users/alice/following",
 "id": "{{R}}/users/alice", "inbox": "{{R}}/users/alice/inbox", "manuallyApprovesFollowers": false, "name": "Alice",
 "outbox": "{{R}}/users/alice/outbox", "preferredUsername": "alice",
 "publicKey": {"id": "{{R}}/users/alice#main-key", "owner": "{{R}}/users/alice", "publicKeyPem": "{{PEM}}"},
 "summary": "Pleroma user", "tag": [], "type": "Person", "url": "{{R}}/users/alice", "vcard:bday": null, "webfinger": "acct:alice@{{RH}}"
}`

const pleromaNote = `{
 "actor": "{{R}}/users/alice", "attachment": [], "attributedTo": "{{R}}/users/alice",
 "cc": ["{{R}}/users/alice/followers"], "content": "{{CONTENT}} <span class=\"h-card\"><a class=\"u-url mention\" data-user=\"9xyz\" href=\"{{L}}/users/bob\" rel=\"ugc\">@<span>bob</span></a></span> <a class=\"hashtag\" data-tag=\"trough\" href=\"{{R}}/tag/trough\" rel=\"tag ugc\">#trough</a>",
 "context": "{{R}}/contexts/abc", "conversation": "{{R}}/contexts/abc", "id": "{{ID}}", "published": "2026-01-01T12:00:00.000000Z",
 "sensitive": null, "source": {"content": "{{CONTENT}} @bob@{{LH}} #trough", "mediaType": "text/plain"}, "summary": "",
 "tag": [{"href": "{{L}}/users/bob", "name": "@bob@{{LH}}", "type": "Mention"}, {"href": "{{R}}/tags/trough", "name": "#trough", "type": "Hashtag"}],
 "to": ["https://www.w3.org/ns/activitystreams#Public", "{{L}}/users/bob"], "type": "Note"
}`

// Pixelfed: photo posts.
const pixelfedActor = `{
 "@context": ["https://w3id.org/security/v1", "https://www.w3.org/ns/activitystreams", {"toot": "http://joinmastodon.org/ns#", "manuallyApprovesFollowers": "as:manuallyApprovesFollowers", "alsoKnownAs": {"@id": "as:alsoKnownAs", "@type": "@id"}, "movedTo": {"@id": "as:movedTo", "@type": "@id"}, "indexable": "toot:indexable", "suspended": "toot:suspended"}],
 "id": "{{R}}/users/alice", "type": "Person", "following": "{{R}}/users/alice/following", "followers": "{{R}}/users/alice/followers",
 "inbox": "{{R}}/users/alice/inbox", "outbox": "{{R}}/users/alice/outbox", "preferredUsername": "alice", "name": "Alice",
 "summary": "Pixelfed user", "url": "{{R}}/alice", "manuallyApprovesFollowers": false, "indexable": true,
 "publicKey": {"id": "{{R}}/users/alice#main-key", "owner": "{{R}}/users/alice", "publicKeyPem": "{{PEM}}"},
 "icon": {"type": "Image", "mediaType": "image/jpeg", "url": "{{R}}/storage/avatars/default.jpg"},
 "endpoints": {"sharedInbox": "{{R}}/f/inbox"}
}`

const pixelfedNote = `{
 "@context": ["https://w3id.org/security/v1", "https://www.w3.org/ns/activitystreams"],
 "id": "{{ID}}", "type": "Note", "summary": null, "content": "{{CONTENT}} <a href=\"{{L}}/@bob\" class=\"u-url mention\">@bob@{{LH}}</a> <a href=\"{{R}}/discover/tags/trough?src=hash\" title=\"#trough\" class=\"u-url hashtag\" rel=\"external nofollow noopener\">#trough</a>",
 "inReplyTo": null, "published": "2026-01-01T12:00:00+00:00", "url": "{{R}}/p/alice/1", "attributedTo": "{{R}}/users/alice",
 "to": ["https://www.w3.org/ns/activitystreams#Public"], "cc": ["{{R}}/users/alice/followers", "{{L}}/users/bob"], "sensitive": false,
 "attachment": [{"type": "Image", "mediaType": "image/jpeg", "url": "{{R}}/storage/m/1.jpg", "name": "sunset", "blurhash": "U9F", "width": 1080, "height": 1080}],
 "tag": [{"type": "Mention", "href": "{{L}}/users/bob", "name": "@bob@{{LH}}"}, {"type": "Hashtag", "href": "{{R}}/discover/tags/trough", "name": "#trough"}],
 "commentsEnabled": true, "capabilities": {"announce": "https://www.w3.org/ns/activitystreams#Public", "like": "https://www.w3.org/ns/activitystreams#Public", "reply": "https://www.w3.org/ns/activitystreams#Public"},
 "location": null
}`

// Friendica: hs2019 with (created), diaspora extensions.
const friendicaActor = `{
 "@context": ["https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1", {"vcard": "http://www.w3.org/2006/vcard/ns#", "dfrn": "http://purl.org/macgirvin/dfrn/1.0/", "diaspora": "https://diasporafoundation.org/ns/", "litepub": "http://litepub.social/ns#", "toot": "http://joinmastodon.org/ns#", "featured": {"@id": "toot:featured", "@type": "@id"}, "schema": "http://schema.org#", "manuallyApprovesFollowers": "as:manuallyApprovesFollowers", "sensitive": "as:sensitive", "Hashtag": "as:Hashtag", "directMessage": "litepub:directMessage", "discoverable": "toot:discoverable", "PropertyValue": "schema:PropertyValue", "value": "schema:value"}],
 "id": "{{R}}/profile/alice", "diaspora:guid": "b5a9", "type": "Person", "following": "{{R}}/following/alice", "followers": "{{R}}/followers/alice",
 "inbox": "{{R}}/inbox/alice", "outbox": "{{R}}/outbox/alice", "featured": "{{R}}/featured/alice", "preferredUsername": "alice", "name": "Alice",
 "vcard:hasAddress": {"@type": "vcard:Home", "vcard:country-name": "", "vcard:region": "", "vcard:locality": ""},
 "summary": "Friendica user", "url": "{{R}}/profile/alice", "manuallyApprovesFollowers": false, "discoverable": true,
 "publicKey": {"id": "{{R}}/profile/alice#main-key", "owner": "{{R}}/profile/alice", "publicKeyPem": "{{PEM}}"},
 "endpoints": {"sharedInbox": "{{R}}/inbox"}, "icon": {"type": "Image", "url": "{{R}}/photo/profile/alice.jpg"}, "generator": {"type": "Application", "name": "Friendica 'Yellow Archangel' 2024.12"}
}`

const friendicaNote = `{
 "id": "{{ID}}", "type": "Note", "summary": null, "inReplyTo": "", "diaspora:guid": "b5a9-1",
 "published": "2026-01-01T12:00:00Z", "updated": "2026-01-01T12:00:00Z", "url": "{{ID}}", "attributedTo": "{{R}}/profile/alice",
 "sensitive": false, "context": "{{R}}/objects/b5a9-1#context", "conversation": "tag:{{RH}},2026:b5a9",
 "content": "{{CONTENT}} @<a href=\"{{L}}/@bob\" class=\"u-url mention\">bob</a> #<a href=\"{{R}}/search?tag=trough\" class=\"mention hashtag\" rel=\"tag\">trough</a>",
 "contentMap": {"en": "{{CONTENT}}"}, "source": {"content": "{{CONTENT}} @[url={{L}}/users/bob]bob[/url] #trough", "mediaType": "text/bbcode"},
 "to": ["https://www.w3.org/ns/activitystreams#Public", "{{L}}/users/bob"], "cc": ["{{R}}/followers/alice"],
 "tag": [{"type": "Mention", "href": "{{L}}/users/bob", "name": "@bob@{{LH}}"}, {"type": "Hashtag", "href": "{{R}}/search?tag=trough", "name": "#trough"}],
 "attachment": [], "generator": {"type": "Application", "name": "Friendica"}
}`

// WordPress ActivityPub plugin: trailing slashes, Articles with a title.
const wordpressActor = `{
 "@context": ["https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1", "https://purl.archive.org/socialweb/webfinger", {"schema": "http://schema.org#", "toot": "http://joinmastodon.org/ns#", "webfinger": {"@id": "https://purl.archive.org/socialweb/webfinger#webfinger", "@type": "xsd:string"}, "featured": {"@id": "toot:featured", "@type": "@id"}, "PropertyValue": "schema:PropertyValue", "value": "schema:value", "manuallyApprovesFollowers": "as:manuallyApprovesFollowers", "discoverable": "toot:discoverable", "indexable": "toot:indexable"}],
 "id": "{{R}}/author/alice/", "type": "Person", "name": "Alice", "summary": "<p>A blog</p>", "url": "{{R}}/author/alice/",
 "icon": {"type": "Image", "url": "{{R}}/wp-content/avatar.png"}, "published": "2020-01-01T00:00:00Z",
 "following": "{{R}}/wp-json/activitypub/1.0/actors/1/following", "followers": "{{R}}/wp-json/activitypub/1.0/actors/1/followers",
 "inbox": "{{R}}/wp-json/activitypub/1.0/actors/1/inbox", "outbox": "{{R}}/wp-json/activitypub/1.0/actors/1/outbox",
 "preferredUsername": "alice", "endpoints": {"sharedInbox": "{{R}}/wp-json/activitypub/1.0/inbox"},
 "publicKey": {"id": "{{R}}/author/alice/#main-key", "owner": "{{R}}/author/alice/", "publicKeyPem": "{{PEM}}"},
 "manuallyApprovesFollowers": false, "discoverable": true, "indexable": true, "webfinger": "alice@{{RH}}"
}`

const wordpressArticle = `{
 "@context": ["https://www.w3.org/ns/activitystreams", {"Hashtag": "as:Hashtag"}],
 "id": "{{ID}}", "type": "Article", "attachment": [], "attributedTo": "{{R}}/author/alice/",
 "content": "<p>{{CONTENT}}</p><p>Hi <a href=\"{{L}}/@bob\">@bob</a></p>", "contentMap": {"en": "<p>{{CONTENT}}</p>"},
 "name": "My first blog post", "summary": "A short summary", "published": "2026-01-01T12:00:00Z", "url": "{{ID}}",
 "to": ["https://www.w3.org/ns/activitystreams#Public", "{{R}}/wp-json/activitypub/1.0/actors/1/followers"],
 "cc": ["{{L}}/users/bob"], "mediaType": "text/html", "sensitive": false,
 "tag": [{"type": "Hashtag", "href": "{{R}}/tag/trough/", "name": "#trough"}, {"type": "Mention", "href": "{{L}}/users/bob", "name": "@bob@{{LH}}"}]
}`

// A Fedify-based server (Hollo, Ghost, ...) signing with RFC 9421.
const fedifyActor = `{
 "@context": ["https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1", "https://w3id.org/security/data-integrity/v1", "https://www.w3.org/ns/did/v1", "https://w3id.org/security/multikey/v1", {"toot": "http://joinmastodon.org/ns#", "discoverable": "toot:discoverable", "manuallyApprovesFollowers": "as:manuallyApprovesFollowers"}],
 "id": "{{R}}/@alice", "type": "Person", "preferredUsername": "alice", "name": "Alice", "summary": "<p>Hollo user</p>",
 "url": "{{R}}/@alice", "inbox": "{{R}}/@alice/inbox", "outbox": "{{R}}/@alice/outbox", "followers": "{{R}}/@alice/followers",
 "following": "{{R}}/@alice/following", "endpoints": {"type": "as:Endpoints", "sharedInbox": "{{R}}/inbox"},
 "publicKey": {"id": "{{R}}/@alice#main-key", "type": "CryptographicKey", "owner": "{{R}}/@alice", "publicKeyPem": "{{PEM}}"},
 "assertionMethod": [{"id": "{{R}}/@alice#multikey-1", "type": "Multikey", "controller": "{{R}}/@alice", "publicKeyMultibase": "z6MkiTBz1ymuepAQ4HEHYSF1H8quG5GLVVQR3djdX3mDooWp"}],
 "manuallyApprovesFollowers": false, "discoverable": true
}`

const fedifyNote = `{
 "@context": ["https://www.w3.org/ns/activitystreams", "https://w3id.org/security/data-integrity/v1"],
 "id": "{{ID}}", "type": "Note", "attributedTo": "{{R}}/@alice", "content": "<p>{{CONTENT}} <a href=\"{{L}}/users/bob\" class=\"u-url mention\">@bob@{{LH}}</a> <a href=\"{{R}}/tags/trough\" class=\"mention hashtag\" rel=\"tag\">#trough</a></p>",
 "contentMap": {"en": "<p>{{CONTENT}}</p>"}, "published": "2026-01-01T12:00:00Z", "url": "{{ID}}",
 "to": "as:Public", "cc": ["{{R}}/@alice/followers", "{{L}}/users/bob"],
 "tag": [{"type": "Mention", "href": "{{L}}/users/bob", "name": "@bob@{{LH}}"}, {"type": "Hashtag", "href": "{{R}}/tags/trough", "name": "#trough"}],
 "sensitive": false
}`

var flavors = []flavor{
	{name: "mastodon", style: sigCavage, actorPath: "/users/alice", actor: mastodonActor, keyID: "{{R}}/users/alice#main-key",
		notePath: "/users/alice/statuses/", note: mastodonNote, wantAtt: "image/png", wantEmoji: "blobcat", wantTag: "trough"},
	{name: "gotosocial", style: sigHS2019, actorPath: "/users/alice", actor: gtsActor, keyID: "{{R}}/users/alice/main-key",
		docs: map[string]string{"/users/alice/main-key": gtsKeyStub}, notePath: "/users/alice/statuses/", note: gtsNote,
		wantAtt: "image/jpeg", wantTag: "trough",
		tweak: func(act obj) { // GTS writes single-valued fields without arrays
			if to, ok := act["to"].([]any); ok && len(to) == 1 {
				act["to"] = to[0]
			}
		}},
	{name: "misskey", style: sigCavage, actorPath: "/users/9h3k2l1abc", actor: misskeyActor, keyID: "{{R}}/users/9h3k2l1abc#main-key",
		notePath: "/notes/", note: misskeyNote, wantAtt: "image/webp", wantEmoji: "misskey", wantTag: "trough",
		likeExtra: map[string]any{"content": "🍮", "_misskey_reaction": "🍮"}},
	{name: "pleroma", style: sigPleroma, actorPath: "/users/alice", actor: pleromaActor, keyID: "{{R}}/users/alice#main-key",
		notePath: "/objects/", note: pleromaNote, wantTag: "trough"},
	{name: "pixelfed", style: sigCavageCT, actorPath: "/users/alice", actor: pixelfedActor, keyID: "{{R}}/users/alice#main-key",
		notePath: "/p/alice/", note: pixelfedNote, wantAtt: "image/jpeg", wantTag: "trough"},
	{name: "friendica", style: sigCreated, actorPath: "/profile/alice", actor: friendicaActor, keyID: "{{R}}/profile/alice#main-key",
		notePath: "/objects/", note: friendicaNote, wantTag: "trough"},
	{name: "wordpress", style: sigAuthHeader, actorPath: "/author/alice/", actor: wordpressActor, keyID: "{{R}}/author/alice/#main-key",
		notePath: "/?p=", note: wordpressArticle, wantTag: "trough"},
	{name: "fedify-rfc9421", style: sigRFC9421, actorPath: "/@alice", actor: fedifyActor, keyID: "{{R}}/@alice#main-key",
		notePath: "/@alice/", note: fedifyNote, wantTag: "trough"},
}

func TestCompatibility(t *testing.T) {
	for _, f := range flavors {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			runScenario(t, f)
		})
	}
}

func runScenario(t *testing.T, f flavor) {
	la := startLiveApp(t)
	ctx := context.Background()
	bob, err := la.CreateLocalUser("bob", "password123", false)
	if err != nil {
		t.Fatal(err)
	}
	r := newRemote(t, la)
	r.put(f.actorPath, f.actor)
	for p, d := range f.docs {
		r.put(p, d)
	}
	actorID := r.Base + f.actorPath
	keyID := r.expand(f.keyID)
	send := func(act obj) {
		t.Helper()
		if f.tweak != nil {
			f.tweak(act)
		}
		if code := r.send(act, keyID, f.style); code != 202 {
			t.Fatalf("%s rejected with %d", ap.Type(act), code)
		}
	}
	act := func(typ string, object any) obj {
		return obj{"@context": asContext, "id": r.Base + "/activities/" + itoa(r.next()), "type": typ, "actor": actorID, "object": object}
	}

	// 1. they follow us: the follow is stored and a signed Accept comes back
	follow := act("Follow", bob.URI)
	send(follow)
	eventually(t, "Accept delivered to remote", func() bool { return len(r.receivedOf("Accept")) > 0 })
	acc := r.receivedOf("Accept")[0]
	if acc.SigErr != nil {
		t.Fatalf("our Accept failed independent signature verification: %v", acc.SigErr)
	}
	// servers that sign with RFC 9421 get RFC 9421 back, the rest draft-cavage
	if got := acc.Header.Get("Signature-Input") != ""; got != (f.style == sigRFC9421) {
		t.Fatalf("Accept signed with RFC 9421 = %v for a %s sender", got, f.style)
	}
	if ap.ID(ap.Object(acc.Activity["object"])["id"]) != follow["id"] && ap.ID(acc.Activity["object"]) != follow["id"] {
		t.Fatalf("Accept does not reference the Follow: %v", acc.Activity["object"])
	}
	remoteAcc, err := la.AccountByURI(actorID)
	if err != nil {
		t.Fatalf("remote actor not stored: %v", err)
	}
	if remoteAcc.Username != "alice" || remoteAcc.Domain != r.Host {
		t.Fatalf("actor stored as %s@%s", remoteAcc.Username, remoteAcc.Domain)
	}
	if !la.relationship(remoteAcc.ID, bob.ID).Following {
		t.Fatal("follow not stored as accepted")
	}

	// 2. we follow them and they accept
	if err := la.Follow(bob, remoteAcc); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Follow delivered", func() bool { return len(r.receivedOf("Follow")) > 0 })
	ourFollow := r.receivedOf("Follow")[0]
	if ourFollow.SigErr != nil {
		t.Fatalf("our Follow failed signature verification: %v", ourFollow.SigErr)
	}
	send(act("Accept", ourFollow.Activity))
	eventually(t, "our follow accepted", func() bool { return la.relationship(bob.ID, remoteAcc.ID).Following })

	// 3. they post (mentioning us, with a hashtag, maybe media and emoji)
	noteID := r.Base + f.notePath + itoa(r.next())
	tmpl := strings.ReplaceAll(strings.ReplaceAll(f.note, "{{ID}}", noteID), "{{CONTENT}}", "hello from "+f.name)
	note := mustJSON(t, r.expand(tmpl))
	r.put(strings.TrimPrefix(noteID, r.Base), r.expand(tmpl))
	create := act("Create", note)
	create["to"], create["cc"] = note["to"], note["cc"]
	send(create)
	var st *Status
	eventually(t, "post stored", func() bool { st, err = la.StatusByURI(noteID); return err == nil })
	full, err := la.loadStatus(bob, st.ID)
	if err != nil {
		t.Fatalf("post not visible to bob: %v", err)
	}
	if !strings.Contains(full.Content, "hello from "+f.name) {
		t.Errorf("content = %q", full.Content)
	}
	if full.Account.ID != remoteAcc.ID {
		t.Errorf("post attributed to %s", full.Account.URI)
	}
	if f.wantAtt != "" && (len(full.Attachments) != 1 || full.Attachments[0].MediaType != f.wantAtt) {
		t.Errorf("attachments = %+v, want one %s", full.Attachments, f.wantAtt)
	}
	if f.wantTag != "" && (len(full.Tags) == 0 || full.Tags[0] != f.wantTag) {
		t.Errorf("tags = %v, want %s", full.Tags, f.wantTag)
	}
	if f.wantEmoji != "" && (len(full.Emojis) == 0 || full.Emojis[0].Shortcode != f.wantEmoji) {
		t.Errorf("emojis = %v, want %s", full.Emojis, f.wantEmoji)
	}
	mentioned := false
	for _, m := range full.Mentions {
		mentioned = mentioned || m.ID == bob.ID
	}
	if !mentioned {
		t.Errorf("bob not recorded as mentioned")
	}
	eventually(t, "mention notification", func() bool {
		return la.countInt(`SELECT COUNT(*) FROM notifications WHERE account_id=? AND type='mention' AND status_id=?`, bob.ID, st.ID) == 1
	})
	if strings.Contains(string(la.renderHTML(full.Content, full.Mentions, full.Emojis)), "<script") {
		t.Error("unsanitized content")
	}

	// 4. our post reaches them, then they like and boost it
	mine, err := la.PostStatus(ctx, bob, PostInput{Text: "a local post #hello", Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "Create delivered", func() bool { return len(r.receivedOf("Create")) > 0 })
	cr := r.receivedOf("Create")[0]
	if cr.SigErr != nil {
		t.Fatalf("our Create failed signature verification: %v", cr.SigErr)
	}
	checkOurNote(t, la, ap.Object(cr.Activity["object"]), mine)

	like := act("Like", mine.URI)
	for k, v := range f.likeExtra {
		like[k] = v
	}
	send(like)
	eventually(t, "like stored", func() bool { return la.countInt(`SELECT COUNT(*) FROM likes WHERE status_id=?`, mine.ID) == 1 })
	eventually(t, "like notification", func() bool {
		return la.countInt(`SELECT COUNT(*) FROM notifications WHERE type='favourite' AND status_id=?`, mine.ID) == 1
	})
	announce := act("Announce", mine.URI)
	announce["to"], announce["cc"] = []any{ap.Public}, []any{bob.URI, actorID + "/followers"}
	send(announce)
	eventually(t, "boost stored", func() bool { return la.countInt(`SELECT COUNT(*) FROM statuses WHERE boost_of_id=?`, mine.ID) == 1 })

	// 5. they edit their post
	note["content"] = "<p>edited by " + f.name + "</p>"
	note["updated"] = "2026-01-02T00:00:00Z"
	upd := act("Update", note)
	send(upd)
	eventually(t, "edit applied", func() bool {
		s, err := la.StatusByID(st.ID)
		return err == nil && strings.Contains(s.Content, "edited by") && !s.EditedAt.IsZero()
	})

	// 6. they undo the like and the boost
	send(act("Undo", like))
	send(act("Undo", announce))
	eventually(t, "like undone", func() bool { return la.countInt(`SELECT COUNT(*) FROM likes WHERE status_id=?`, mine.ID) == 0 })
	eventually(t, "boost undone", func() bool { return la.countInt(`SELECT COUNT(*) FROM statuses WHERE boost_of_id=?`, mine.ID) == 0 })

	// 7. they delete their post (Tombstone form)
	send(act("Delete", obj{"id": noteID, "type": "Tombstone"}))
	eventually(t, "post deleted", func() bool { _, err := la.StatusByURI(noteID); return err != nil })

	// 8. profile update
	actorDoc := mustJSON(t, r.expand(f.actor))
	actorDoc["name"] = "Alice Renamed"
	send(act("Update", actorDoc))
	eventually(t, "profile updated", func() bool { a, err := la.AccountByURI(actorID); return err == nil && a.DisplayName == "Alice Renamed" })

	// 9. they unfollow us
	send(act("Undo", follow))
	eventually(t, "unfollowed", func() bool { return !la.relationship(remoteAcc.ID, bob.ID).Following })

	// 10. the account is deleted
	r.put(f.actorPath, "GONE")
	send(act("Delete", actorID))
	eventually(t, "account removed", func() bool { _, err := la.AccountByURI(actorID); return err != nil })
}

// checkOurNote validates what we send against what Mastodon and friends require.
func checkOurNote(t *testing.T, la *liveApp, note obj, st *Status) {
	t.Helper()
	if ap.Str(note, "id") != st.URI || ap.Type(note) != "Note" {
		t.Fatalf("unexpected object %v", note)
	}
	if !ap.SameHost(ap.Str(note, "id"), ap.Str(note, "attributedTo")) {
		t.Error("note id and attributedTo on different hosts")
	}
	if ap.ParseTime(ap.Str(note, "published")).IsZero() {
		t.Error("note without valid published date")
	}
	if !strings.Contains(ap.Str(note, "content"), "hashtag") {
		t.Error("hashtag not linked in content")
	}
	foundTag := false
	for _, tag := range ap.Objects(note["tag"]) {
		if ap.Type(tag) == "Hashtag" && ap.Str(tag, "name") == "#hello" && strings.HasPrefix(ap.Str(tag, "href"), la.base) {
			foundTag = true
		}
	}
	if !foundTag {
		t.Errorf("Hashtag tag missing: %v", note["tag"])
	}
	public := false
	for _, to := range ap.IDs(note["to"]) {
		public = public || to == ap.Public
	}
	if !public {
		t.Error("public post not addressed to as:Public")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
