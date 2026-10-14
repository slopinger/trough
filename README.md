# trough

A small, single-binary **fediverse (ActivityPub) server** with a built-in web interface.
Pigs eat slop from a trough; this one feeds you posts from the fediverse.

It is modelled on [GoToSocial](https://gotosocial.org/): one Go binary, SQLite, low resource use,
federating with Mastodon, GoToSocial, Misskey, Pleroma/Akkoma and friends. Unlike GoToSocial it doesn't
implement the Mastodon client API. Instead it ships its own server-rendered web interface that works
without JavaScript; a little JS only adds conveniences.

Running instance: **https://fedi.freifick.net**

## Features

**Federation**
- WebFinger (`/.well-known/webfinger`), host-meta, NodeInfo 2.0/2.1, a minimal `/api/v1/instance`
- Actors with RSA keys; HTTP Signatures (draft-cavage, `rsa-sha256`/`hs2019`) on every outgoing request,
  verified on every incoming inbox POST (digest, date window, key ownership, re-fetch on key rotation)
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

**Web interface**
- Home, local and federated timelines, hashtag timelines, threads with nested replies
- Compose with content warnings, four visibilities (public, unlisted, followers-only, direct),
  up to 4 images with alt text, @mentions (resolved via WebFinger) and #hashtags
- Like, boost, reply and delete; likes and boosts update in place with JS, or via plain forms without it
- Profiles with header, avatar, bio, profile fields, locked accounts and bot flag; follow, unfollow and block
- Notifications (mentions, replies, likes, boosts, follows, follow requests), follow request management
- Search by `@user@server`, by profile/post URL, by `#tag`, or full text over known posts
- Admin page: create users, reset passwords, instance title/description, domain blocks (including
  subdomains), federation stats and failing deliveries
- Media: uploads are re-encoded, which strips EXIF metadata such as GPS positions while keeping the
  orientation. Remote media goes through an HMAC-signed caching proxy, so remote servers never see visitors'
  IP addresses and the page can use a strict CSP (`img-src 'self'`).
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

Tests: `go test ./...` for unit tests. Run `python3 test/run-federation.py` to build trough and
exercise two disposable instances end to end: follow/accept, follow requests, posts, mentions, likes,
boosts, replies, visibility, DMs, profile updates, deletes and blocks. The runner uses randomly selected
loopback ports and temporary databases, stops both processes, and removes its data on exit. It does
not use the live instance or its credentials.

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

Back up `/var/lib/trough` (SQLite database plus uploaded media; `cache/` can be dropped).

## Code map

| File | What |
|---|---|
| `main.go` | CLI, configuration |
| `app.go` | app setup, instance actor, local users, workers |
| `db.go`, `models.go` | SQLite schema/migrations, accounts, statuses, relationships |
| `httpsig.go` | HTTP Signatures |
| `fetch.go` | SSRF-safe HTTP client, signed fetches, actors, keys, WebFinger client |
| `ap.go`, `outbound.go` | ActivityStreams helpers, JSON for actors/notes/activities, addressing |
| `inbox.go`, `remote.go` | incoming activities, storing remote posts, backfill, URL resolution |
| `deliver.go` | outgoing delivery queue |
| `actions.go` | what local users do (post, like, boost, follow, block, ...) |
| `timeline.go` | timeline queries, visibility rules, threads, notifications |
| `fedserver.go` | federation HTTP endpoints |
| `web.go`, `admin.go`, `render.go`, `routes.go` | web interface |
| `text.go` | post formatting and HTML sanitizing |
| `media.go` | uploads, EXIF stripping, media proxy |
| `templates/`, `static/` | HTML templates, CSS, JS (embedded in the binary) |

## Not (yet) implemented

Mastodon client API/OAuth (so no third-party apps), voting in polls, editing posts, pinned posts,
mutes and filters, reports (`Flag`), account migration (`Move`), streaming updates, sign-ups,
RFC 9421 signatures.

## License

MIT
