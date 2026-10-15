# Features

## Federation
- WebFinger (`/.well-known/webfinger`), host-meta, NodeInfo 2.0/2.1, a minimal `/api/v1/instance`
- ActivityPub client-to-server: with an OAuth token (`read`/`write`, from `/oauth/token`; the actor's
  `endpoints` advertise both OAuth endpoints) a client can POST `Create`/bare `Note`, `Update`, `Delete`,
  `Like`, `Announce`, `Follow`, `Block` and `Undo` to the user's outbox (`201` with `Location`), upload
  media to `endpoints.uploadMedia` (multipart `file` + `object`, then attach it by URL), and read their
  inbox (home timeline as activities). Addressing picks the visibility; addressed or tagged actors
  are mentioned.
- Actors with RSA keys; HTTP Signatures (draft-cavage, `rsa-sha256`/`hs2019`) on every outgoing request,
  verified on every incoming inbox POST (digest, date window, key ownership, re-fetch on key rotation).
  RFC 9421 HTTP Message Signatures (Fedify-based servers such as Hollo and Ghost) are verified too, and
  deliveries and signed fetches to a host that has sent us a valid RFC 9421 signature are signed with
  RFC 9421 (`@method @target-uri @authority date`, plus `content-type content-digest` on POSTs). If that
  host answers 401/403 we retry with draft-cavage and stick to it for 30 days (double-knocking, as
  Fedify does).
- A signed instance actor (`/actor`) for fetches, so servers in "authorized fetch" mode work
- Inbox (shared and per-user) handling `Follow`, `Accept`, `Reject`, `Undo`, `Create`, `Update`, `Delete`,
  `Like`, `Announce`, `Block`
- Outbox, followers/following and featured collections, and dereferenceable statuses with
  visibility enforced via the requester's signature
- Persistent delivery queue with exponential backoff (about 3 days of retries)
- Only stores what's relevant: posts by people someone here follows, posts that mention or address local
  users, and replies to known posts. Thread parents are fetched on demand, and recent posts are backfilled
  from the outbox when you follow someone or open their profile.
- SSRF protection: remote fetches never connect to loopback/private/link-local addresses
- Remote HTML is sanitized (bluemonday); custom emoji, polls (read-only), content warnings,
  sensitive media, Articles and other object types are displayed
- Groups: Lemmy communities and similar groups that relay posts as `Announce{Create}` show up as boosts by
  the group; relayed activities are always re-fetched from their origin. PeerTube videos (channel +
  account authors, Markdown descriptions, thumbnails) and Lemmy link posts display properly.
- Tested for compatibility ([development.md](development.md#tests)) with Mastodon, GoToSocial, Misskey/Sharkey, Pleroma/Akkoma,
  Pixelfed, Friendica, WordPress, Fedify, Lemmy and PeerTube payloads and signature styles

## Mastodon client API
- OAuth 2: app registration, authorization code with PKCE (consent page, out-of-band codes), client
  credentials and password grants, revocation, `/.well-known/oauth-authorization-server`; connected apps
  can be revoked in Settings
- Reports itself as Mastodon 4.5.0 with `api_versions.mastodon` 7: everything the Mastodon docs list up to
  API version 7 is there (quote posts with policies, featured accounts, attribution domains, blur filters,
  media deletion, edit notifications)
- Accounts (also several at once), statuses (post with media/polls/quotes, edit, delete keeping media for
  redraft unless `delete_media`, several at once, context, source, history), favourites,
  reblogs, bookmarks, pins, reactions (Pleroma-style), polls, media upload (v1/v2), home/public/hashtag
  timelines, notifications (with type filters, plus Mastodon 4.3 grouped notifications at
  `/api/v2/notifications`: likes/boosts per post and follows per day collapse into groups), relationships, follow/block/mute, follow requests, search v2
  with remote resolving, markers, filters (v1 and v2: titles, keywords, contexts, hide/warn/blur, expiry), conversations (direct-message threads with unread state, mark
  read, hide), reports (with forwarding), quote settings (`quote_approval`, `quote_approval_policy`,
  `interaction_policy`, revoking quotes, default policy), endorsements (featured accounts),
  `attribution_domains`, instance v1/v2, CORS
  for browser clients
- Mastodon-style pagination (`max_id`/`since_id`/`min_id`, `Link` headers) and Idempotency-Key
- Streaming API over WebSocket (`/api/v1/streaming`) and server-sent events (`/api/v1/streaming/user`, ...)
- Web Push (`/api/v1/push/subscription`, VAPID, RFC 8291 encryption) so mobile and browser apps get
  notifications while closed
- Checked with the Mastodon.py client library, the toot CLI, the iOS apps Ice Cubes and Mastodon
  (their own networking/model code built on Linux), Phanpy and Elk in a browser and the Android app
  Tusky 33.1 in an emulator; see [development.md](development.md#client-tests)

## Web interface
- Home, local and federated timelines, hashtag timelines, threads with nested replies
- Compose with content warnings, four visibilities (public, unlisted, followers-only, direct),
  up to 4 images with alt text, @mentions (resolved via WebFinger) and #hashtags
- **AI writing (optional):** "Write with AI" in the compose box drafts a post from a short instruction,
  improves the current draft, or drafts a reply with the thread as context. The draft streams into the
  text box and is never posted automatically. Uses DeepSeek's chat completions API (any
  OpenAI-compatible endpoint works); remote post text is passed to the model as delimited data, not
  instructions. 30 requests per user and hour; admins switch it on and off and pick the model on `/admin`.
  The same model translates posts for Mastodon apps (`POST /api/v1/statuses/:id/translate`, cached per
  post version and language; the instance advertises translation while AI is on).
- Private notes on other people's profiles (also `POST /api/v1/accounts/:id/note` for apps)
- Edit your posts (with an edit history), pin up to 5 to your profile (featured collection, `Add`/`Remove`), bookmarks
- Polls (2-4 options, single or multiple choice) federated as `Question`; vote in remote polls; poll results
  and end notifications
- Quote posts (FEP-044f `quote`, `quoteUrl`, `_misskey_quote`, FEP-e232 links) shown as embedded posts,
  with consent as Mastodon 4.5+ does it (FEP-044f): posts publish `interactionPolicy.canQuote` (anyone,
  followers or only the author; a per-account default and per post), `QuoteRequest`s for our posts are
  answered with `Accept` + a `QuoteAuthorization` stamp (served under `/users/<name>/quote_authorizations/`)
  or `Reject`, quoting a post from such a server asks it first and stays pending until approved, and
  approvals can be revoked either way. Quotes from software without the handshake work as before.
  Edits notify those who boosted (`update`) or quoted (`quoted_update`) a post;
  emoji reactions (Misskey reactions, `EmojiReact`) shown as chips, and you can react yourself
- Scheduled posts (compose box "Schedule", a Scheduled page, and `scheduled_at` / `/api/v1/scheduled_statuses`)
- Link previews (OpenGraph cards), fetched in the background
- Who to follow (Explore → People, `/api/v2/suggestions` with dismissing): accounts followed by people you
  follow, then popular ones here; profiles show which people you follow follow them
  (`/api/v1/accounts/familiar_followers`)
- Explore page and Mastodon trends API (`/api/v1/trends/{tags,statuses,links}`): posts with the most
  likes, boosts and replies from the last three days, and hashtags and links used by at least two people
  this week, with per-day history (also in every hashtag entity)
- Custom emoji uploaded by admins, usable as `:shortcode:` in posts, names and bios, federated as `Emoji` tags
- Featured accounts on profiles (Mastodon endorsements, not federated, like on Mastodon); attribution
  domains (`attributionDomains`) for websites that credit you with `fediverse:creator`
- Blocking whole servers for yourself (Settings, a profile's "Block server" button, `/api/v1/domain_blocks`):
  hides their posts, boosts of them and notifications, removes followers there and refuses new ones;
  the list of servers the instance blocks is public (`/api/v1/instance/domain_blocks`)
- Mutes, keyword filters (collapse, hide media or hide; through the API also grouped, per context and expiring), blocks; reports with optional anonymous forwarding (`Flag`)
  and incoming reports for admins
- Account migration: aliases (`alsoKnownAs`), moving followers to a new account (`Move`), and following
  accounts that moved here
- Sign-ups: closed, invite links or open (admin setting); live "new posts" banner and notification badge
- Lists (with replies policy and "hide from Home"), followed hashtags (public posts with them show up in
  Home), and relay subscriptions (Mastodon `/inbox` style and LitePub actor style) on `/admin` to fill the
  federated timeline; relayed posts are always fetched from their origin
- Like, boost, reply and delete; likes and boosts update in place with JS, or via plain forms without it
- Profiles with header, avatar, bio, profile fields, locked accounts and bot flag; follow, unfollow and block
- Notifications (mentions, replies, likes, boosts, follows, follow requests), follow request management
- Search by `@user@server`, by profile/post URL, by `#tag`, or full text over known posts
- Admin page: create users, reset passwords, instance title/description, domain blocks (including
  subdomains), federation stats and failing deliveries
- Server rules (on About and sign-up, `/api/v1/instance/rules`, in the instance entity) and announcements
  (a banner until each user dismisses it; `/api/v1/announcements` with dismissing and emoji reactions)
- Media: uploads are re-encoded, which strips EXIF metadata such as GPS positions while keeping the
  orientation. Remote media goes through an HMAC-signed caching proxy, so remote servers never see visitors'
  IP addresses and the page can use a strict CSP (`img-src 'self'`). Cache entries with missing data
  or invalid metadata are refetched; per-URL locks coordinate fetches and expiry cleanup.
- Light and dark theme, responsive layout with a bottom navigation bar on phones
- Security: bcrypt passwords, login rate limiting, HttpOnly/SameSite session cookies, per-session CSRF
  tokens, Origin checks, strict Content-Security-Policy (no inline scripts or styles)

## Not implemented

ActivityPub C2S activities other than the ones listed above.
