# trough

A small, single-binary **fediverse (ActivityPub) server** with a built-in web interface.
Pigs eat slop from a trough; this one feeds you posts from the fediverse.

It is modelled on [GoToSocial](https://gotosocial.org/): one Go binary, SQLite, low resource use,
federating with Mastodon, GoToSocial, Misskey, Pleroma/Akkoma and friends. Like GoToSocial it speaks the
Mastodon client API, so apps such as Tusky, Ivory, Elk, Phanpy, Ice Cubes or Fedilab work with it, and it
also ships its own server-rendered web interface that works without JavaScript (a little JS only adds
conveniences).

Running instance: **https://fedi.freifick.net**

## Features

**Federation**
- WebFinger (`/.well-known/webfinger`), host-meta, NodeInfo 2.0/2.1, a minimal `/api/v1/instance`
- ActivityPub client-to-server: with an OAuth token (`read`/`write`, from `/oauth/token`; the actor's
  `endpoints` advertise both OAuth endpoints) a client can POST `Create`/bare `Note`, `Update`, `Delete`,
  `Like`, `Announce`, `Follow`, `Block` and `Undo` to the user's outbox (`201` with `Location`) and read
  their inbox (home timeline as activities). Addressing picks the visibility; addressed or tagged actors
  are mentioned.
- Actors with RSA keys; HTTP Signatures (draft-cavage, `rsa-sha256`/`hs2019`) on every outgoing request,
  verified on every incoming inbox POST (digest, date window, key ownership, re-fetch on key rotation).
  RFC 9421 HTTP Message Signatures (Fedify-based servers such as Hollo and Ghost) are verified too, and
  deliveries to a host that has sent us a valid RFC 9421 signature are signed with RFC 9421
  (`@method @target-uri @authority date content-type content-digest`). If that host answers 401/403 we
  retry with draft-cavage and stick to it for 30 days (double-knocking, as Fedify does).
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
- Tested for compatibility (see below) with Mastodon, GoToSocial, Misskey/Sharkey, Pleroma/Akkoma,
  Pixelfed, Friendica, WordPress, Fedify, Lemmy and PeerTube payloads and signature styles

**Mastodon client API**
- OAuth 2: app registration, authorization code with PKCE (consent page, out-of-band codes), client
  credentials and password grants, revocation, `/.well-known/oauth-authorization-server`; connected apps
  can be revoked in Settings
- Accounts, statuses (post with media/polls/quotes, edit, delete, context, source, history), favourites,
  reblogs, bookmarks, pins, reactions (Pleroma-style), polls, media upload (v1/v2), home/public/hashtag
  timelines, notifications (with type filters, plus Mastodon 4.3 grouped notifications at
  `/api/v2/notifications`: likes/boosts per post and follows per day collapse into groups), relationships, follow/block/mute, follow requests, search v2
  with remote resolving, markers, filters, conversations, instance v1/v2, CORS for browser clients
- Mastodon-style pagination (`max_id`/`since_id`/`min_id`, `Link` headers) and Idempotency-Key
- Streaming API over WebSocket (`/api/v1/streaming`) and server-sent events (`/api/v1/streaming/user`, ...)
- Web Push (`/api/v1/push/subscription`, VAPID, RFC 8291 encryption) so mobile and browser apps get
  notifications while closed
- Checked with the Mastodon.py client library (`test/mastodon_py.py`), the toot CLI
  (`nix shell nixpkgs#toot --command bash test/toot.sh`: login, posting, threads, polls, media,
  favourites, boosts, bookmarks, pins, follows, lists, followed tags, mutes, blocks, deletes),
  Phanpy and Elk in a browser, and Go tests (`api_test.go`)

**Web interface**
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
- Edit your posts (with an edit history), pin up to 5 to your profile (featured collection, `Add`/`Remove`), bookmarks
- Polls (2-4 options, single or multiple choice) federated as `Question`; vote in remote polls; poll results
  and end notifications
- Quote posts (FEP-044f `quote`, `quoteUrl`, `_misskey_quote`, FEP-e232 links) shown as embedded posts;
  emoji reactions (Misskey reactions, `EmojiReact`) shown as chips, and you can react yourself
- Scheduled posts (compose box "Schedule", a Scheduled page, and `scheduled_at` / `/api/v1/scheduled_statuses`)
- Link previews (OpenGraph cards), fetched in the background
- Custom emoji uploaded by admins, usable as `:shortcode:` in posts, names and bios, federated as `Emoji` tags
- Mutes, keyword filters (collapse or hide), blocks; reports with optional anonymous forwarding (`Flag`)
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
- Media: uploads are re-encoded, which strips EXIF metadata such as GPS positions while keeping the
  orientation. Remote media goes through an HMAC-signed caching proxy, so remote servers never see visitors'
  IP addresses and the page can use a strict CSP (`img-src 'self'`). Cache entries with missing data
  or invalid metadata are refetched; per-URL locks coordinate fetches and expiry cleanup.
- Light and dark theme, responsive layout with a bottom navigation bar on phones
- Security: bcrypt passwords, login rate limiting, HttpOnly/SameSite session cookies, per-session CSRF
  tokens, Origin checks, strict Content-Security-Policy (no inline scripts or styles)

## Usage

```sh
go build -o trough .

# create an account (prints a random password)
./trough user create -host example.org -data ./data -admin alice

# run (behind a TLS-terminating reverse proxy that forwards to 127.0.0.1:8790)
./trough serve -host example.org -listen 127.0.0.1:8790 -data ./data
```

Other commands: `trough user password <name>`, `trough user admin <name> on|off`, `trough user list`.
Every flag can also be set as an environment variable (`TROUGH_HOST`, `TROUGH_LISTEN`, `TROUGH_DATA`).

The host name is baked into every ID that is handed out, so it can't be changed later. The database
remembers its host and refuses to start with a different one.

### Local development

`-insecure` switches to plain HTTP and allows private addresses, so two instances can federate on
localhost:

```sh
./trough user create -insecure -host 127.0.0.1:8801 -data a alice
./trough user create -insecure -host 127.0.0.1:8802 -data b bob
./trough serve -insecure -host 127.0.0.1:8801 -listen 127.0.0.1:8801 -data a &
./trough serve -insecure -host 127.0.0.1:8802 -listen 127.0.0.1:8802 -data b &
```

Then log in on both and search for `@alice@127.0.0.1:8801` from bob's side.

Behind a local TLS proxy (needed to federate with software that always uses https, such as
GoToSocial), add `-scheme https`: IDs then use https while private addresses stay allowed. Point
`SSL_CERT_FILE` at the proxy's CA so trough trusts it.

AI writing needs an API key: `TROUGH_DEEPSEEK_API_KEY` (or `DEEPSEEK_API_KEY`) in the environment, or one
entered on `/admin`. Optional: `TROUGH_AI_MODEL` (default `deepseek-v4-pro`, also `deepseek-flash`) and
`TROUGH_AI_BASE_URL` (default `https://api.deepseek.com`). On fedi.freifick.net the key lives in the
root-only `/etc/trough-secrets.env`, which `trough.service` loads.

Tests: `go test ./...` for unit tests. The ActivityPub compatibility suite (`compat_*_test.go`) runs a
live instance against a fake remote that reproduces the actor/object shapes and signature styles of
Mastodon, GoToSocial, Misskey, Pleroma, Pixelfed, Friendica, WordPress, Fedify (RFC 9421), Lemmy and
PeerTube through a whole follow/post/like/boost/edit/undo/delete lifecycle, verifying our signatures
with the independent go-fed/httpsig implementation. Run `python3 test/run-federation.py` to build trough and
exercise two disposable instances end to end: follow/accept, follow requests, posts, mentions, likes,
boosts, replies, visibility, DMs, profile updates, deletes and blocks. The runner uses randomly selected
loopback ports and temporary databases, stops both processes, and removes its data on exit. It does
not use the live instance or its credentials.

`python3 test/run-gotosocial.py` federates a disposable trough with a disposable **real GoToSocial**
(from `GOTOSOCIAL=/path/to/binary`, or built with Nix) behind a throwaway Caddy with its own local CA,
then runs `test/gotosocial.py` over both servers' client APIs: follow requests and accepts, posts,
mentions, likes and unlikes, boosts, replies, edits, polls and votes, followers-only posts, DMs, deletes,
profile updates and unfollows, in both directions. It takes about a minute and needs `caddy` on PATH.
Known GoToSocial 0.22.1 issue: deleting a remote post that already has replies fails on its side
(`NOT NULL constraint failed: statuses.thread_id` while stubbing), so that one check only warns.

To test two instances you started yourself, use `python3 test/federation.py <alice-pw> <bob-pw>`.
It defaults to ports 8801/8802; `TROUGH_TEST_A` and `TROUGH_TEST_B` override the base URLs.
The passwords can instead come from `TROUGH_TEST_ALICE_PASSWORD` and `TROUGH_TEST_BOB_PASSWORD`.

The JPEG metadata parser also has a fuzz target:
`go test -run '^$' -fuzz '^FuzzJPEGOrientation$' -fuzztime=30s`.

### Deployment on fedi.freifick.net

`./deploy.sh` runs the tests, builds a static binary, installs it to `/usr/local/bin/trough` along with
`troughctl` (runs admin commands as the service user), the hardened `trough.service` (user `trough`, data in
`/var/lib/trough`, config in `/etc/trough.env`) and the Caddy site config, then restarts the service.

```sh
troughctl user create someone          # new account, prints the password
troughctl user list
journalctl -u trough -f                # logs
```

Back up the SQLite database plus uploaded media; `cache/` can be dropped. On fedi.freifick.net,
[server-backups](https://git.freifick.net/slop/server-backups) handles daily verified online snapshots
with `slop-services-backup.timer`, retaining 14 archives in root-only `/srv/backups/slop-services/`.
That repo documents manual verification and restore. The backups are local/same-disk; no off-site
copy is configured. Never copy a live SQLite file without its WAL or the online backup API.

## Code map

| File | What |
|---|---|
| `main.go` | CLI, configuration |
| `app.go` | app setup, instance actor, local users, workers |
| `db.go`, `models.go` | SQLite schema/migrations, accounts, statuses, relationships |
| `httpsig.go`, `rfc9421.go` | HTTP Signatures (draft-cavage, RFC 9421) |
| `ai.go` | AI drafting of posts and replies |
| `translate.go` | AI translation of posts (Mastodon translate API), instance language endpoints |
| `api_oauth.go`, `api_entities.go`, `api_handlers.go`, `api_stream.go`, `push.go` | Mastodon client API, OAuth, streaming, Web Push |
| `relays.go`, `tags.go`, `lists.go`, `scheduled.go`, `emoji.go` | relays, followed hashtags, lists, scheduled posts, custom emoji |
| `features.go`, `polls.go`, `social.go`, `accounts.go` | edits, pins, bookmarks, mutes, filters, reports, polls, reactions, quotes, link cards, migration, sign-ups |
| `fetch.go` | SSRF-safe HTTP client, signed fetches, actors, keys, WebFinger client |
| `ap.go`, `outbound.go` | ActivityStreams helpers, JSON for actors/notes/activities, addressing |
| `inbox.go`, `remote.go` | incoming activities, storing remote posts, backfill, URL resolution |
| `deliver.go` | outgoing delivery queue |
| `actions.go` | what local users do (post, like, boost, follow, block, ...) |
| `timeline.go` | timeline queries, visibility rules, threads, notifications |
| `fedserver.go` | federation HTTP endpoints |
| `c2s.go` | ActivityPub client-to-server (outbox POST, inbox GET) |
| `api_notifs.go` | grouped notifications (`/api/v2/notifications`) |
| `web.go`, `admin.go`, `render.go`, `routes.go` | web interface |
| `text.go` | post formatting and HTML sanitizing |
| `media.go` | uploads, EXIF stripping, media proxy |
| `templates/`, `static/` | HTML templates, CSS, JS (embedded in the binary) |

## Not (yet) implemented

Signed GETs (fetches) still use draft-cavage only. C2S media uploads (`uploadMedia`), and C2S activities
other than the ones listed above.

## License

MIT
