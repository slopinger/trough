# Development

## Repository layout

The layout follows Go's guide for server projects ([go.dev/doc/modules/layout](https://go.dev/doc/modules/layout)):
the command in `cmd/`, the server's packages in `internal/`, everything that isn't Go next to them.

| Path | What |
|---|---|
| `cmd/trough/` | the `trough` command: `serve`, `user ...`, `fetch` |
| `internal/server/` | the server: federation, Mastodon client API, web UI, storage |
| `internal/ap/` | ActivityStreams JSON helpers, addressing, the JSON-LD context |
| `internal/httpsig/` | HTTP Signatures: draft-cavage and RFC 9421, signing and verifying |
| `internal/ulid/` | time-sortable IDs and their numeric form for clients |
| `web/` | HTML templates and static files (CSS, JS, icons), embedded into the binary |
| `test/interop/` | runs trough against real servers (Mastodon, GoToSocial, Akkoma/Pleroma, Misskey/Sharkey, Lemmy) and two troughs against each other |
| `test/clients/` | drives trough with real clients: Mastodon.py, toot, Ice Cubes' and the Mastodon iOS app's code |
| `deploy/`, `deploy.sh` | systemd unit, Caddy site, `troughctl`, install script for fedi.freifick.net |
| `nix/`, `flake.nix` | Nix package and NixOS module |
| `docs/` | features, development, deployment |

### `internal/server` code map

Everything in `internal/server` hangs off one `App` (database, config, workers), so it is one package.
Files are grouped by prefix:

| Files | What |
|---|---|
| `app.go`, `config.go`, `routes.go` | setup, configuration, workers, the HTTP routes |
| `db.go`, `models.go` | SQLite schema/migrations; accounts, statuses, relationships |
| `actions.go` | what local users do (post, like, boost, follow, block, ...) |
| `timeline.go`, `conversations.go`, `lists.go`, `tags.go` | timelines, visibility rules, threads, notifications, DMs, lists, followed hashtags |
| `social.go`, `features.go`, `polls.go`, `quotes.go`, `accounts.go` | edits, pins, bookmarks, mutes, filters, reports, polls, reactions, quotes and quote approval (FEP-044f), link cards, featured accounts, migration, sign-ups |
| `media.go`, `media_av.go`, `emoji.go`, `text.go` | uploads (EXIF stripping; video/audio recognition and MP4 metadata blanking), media proxy, custom emoji, post formatting and HTML sanitizing |
| `scheduled.go`, `push.go`, `trends.go` | scheduled posts, Web Push, trends and the Explore page |
| `announcements.go` | server rules and announcements |
| `domainblocks.go` | servers users block for themselves |
| `suggestions.go` | who to follow, familiar followers |
| `rss.go` | RSS feeds of profiles and hashtags |
| `csvio.go` | CSV export and import (Mastodon formats) |
| `featuredtags.go` | featured hashtags |
| `directory.go` | profile directory |
| `verify.go` | profile link verification (`rel="me"`) |
| `replies.go` | fetching the replies of remote threads |
| `prefs.go` | posting and reading preferences |
| `cleanup.go` | automated deletion of old posts |
| `sessions.go` | logged-in browsers in Settings |
| `privacy.go` | the privacy policy page and its default text |
| `terms.go` | terms of service versions with effective dates (page, API, admin) |
| `activity.go` | weekly activity (`/api/v1/instance/activity`, admin charts) |
| `signupapproval.go` | sign-ups that wait for an admin's approval |
| `limit.go` | limiting accounts (hidden from non-followers, filtered notifications) |
| `domainlimit.go` | limiting whole servers (`domain_limits`, `notLimitedSQL`) |
| `warnings.go` | moderation warnings (strikes), the account status page and appeals |
| `adminaccount.go` | the admin page of one account: facts, history, moderation notes, direct actions |
| `knownservers.go` | the admin list of known servers |
| `trendreview.go` | hiding and approving trends (`trend_reviews`, `/admin/trends`) |
| `serverappearance.go` | server thumbnail and custom CSS |
| `annualreport.go` | annual reports (year in review page and `/api/v1/annual_reports`) |
| `sever.go` | severed relationships: follows ended by server blocks or suspensions, CSV downloads |
| `embed.go` | embedding posts on other websites (iframe page, oEmbed, `/embed.js`) |
| `twofactor.go` | two-factor authentication (TOTP, recovery codes) |
| `accountdelete.go` | deleting your own account |
| `archive.go` | archive download (zip) |
| `notifpolicy.go` | notification filters and requests |
| `autocomplete.go` | compose suggestions for the web UI |
| `convmute.go` | muted conversations |
| `webpush.go` | browser notifications for the web UI (service worker in `web/static/sw.js`) |
| `languages.go` | post languages offered in the web compose form |
| `moderation.go` | report review and actions (delete posts, suspend accounts, block servers); local and origin-server suspensions |
| `statussearch.go` | post search with Mastodon-style operators (web and API) |
| `editdiff.go` | word-level differences between post versions for the edit history page |
| `remoteinteract.go` | following and interacting from other servers (visitors) |
| `ai.go`, `translate.go` | AI drafting and translation |
| `fed_server.go` | federation endpoints: WebFinger, NodeInfo, actors, collections, objects |
| `fed_inbox.go`, `fed_remote.go` | incoming activities, storing remote posts, backfill, URL resolution |
| `fed_outbound.go`, `fed_deliver.go` | JSON for actors/notes/activities, the delivery queue |
| `fed_fetch.go`, `fed_signing.go` | SSRF-safe HTTP client, signed fetches, keys, signature style per host |
| `fed_relays.go`, `fed_c2s.go` | relays, ActivityPub client-to-server |
| `api_oauth.go`, `api_entities.go`, `api_paging.go` | OAuth, Mastodon JSON entities, pagination |
| `api_instance.go`, `api_accounts.go`, `api_statuses.go`, `api_timelines.go`, `api_notifs.go`, `api_media.go`, `api_search.go`, `api_filters.go`, `api_stream.go` | Mastodon client API by topic, streaming |
| `web.go`, `web_render.go` | web UI page type, middleware, templates |
| `web_auth.go`, `web_timelines.go`, `web_statuses.go`, `web_profiles.go`, `web_notifications.go`, `web_search.go`, `web_settings.go`, `web_admin.go` | web UI pages by topic |

## Running locally

```sh
go build -o trough ./cmd/trough
```

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
`TROUGH_AI_BASE_URL` (default `https://api.deepseek.com`).

## Tests

`go test ./...` and `go vet ./...` are the baseline; `go test -race ./...` for concurrency changes.
They cover the client API (`api_*_test.go`: OAuth with PKCE, posting, timelines, media, notifications,
streaming), federation (`fed_*_test.go`) and the rest. The ActivityPub compatibility suite
(`fed_compat_*_test.go`) runs a live instance against a fake remote that reproduces the actor/object
shapes and signature styles of Mastodon, GoToSocial, Misskey, Pleroma, Pixelfed, Friendica, WordPress,
Fedify (RFC 9421), Lemmy and PeerTube through a whole follow/post/like/boost/edit/undo/delete lifecycle,
verifying our signatures with the independent go-fed/httpsig implementation. Add a flavor or fixture
there when fixing an interop bug.

The JPEG metadata parser has a fuzz target:
`go test ./internal/server -run '^$' -fuzz '^FuzzJPEGOrientation$' -fuzztime=30s`.

### Interop tests (`test/interop/`)

These build trough and run it against other software. None of them use the live instance or its
credentials; everything is disposable and removed on exit.

- `python3 test/interop/run-federation.py`: two trough instances on random loopback ports, driven
  through the web UI: follow/accept, follow requests, posts, mentions, likes, boosts, replies,
  visibility, DMs, profile updates, deletes and blocks. To test two instances you started yourself:
  `python3 test/interop/federation.py <alice-pw> <bob-pw>` (ports 8801/8802; `TROUGH_TEST_A`/`_B`
  override the URLs, `TROUGH_TEST_ALICE_PASSWORD`/`_BOB_PASSWORD` the passwords).
- `python3 test/interop/run-gotosocial.py`: a real **GoToSocial** (`GOTOSOCIAL=/path/to/binary`, or
  built with Nix) behind a throwaway Caddy with its own CA, then `test/interop/interop.py` over both
  servers' client APIs: follow requests, posts, mentions, likes, boosts, replies, edits, polls,
  followers-only posts, DMs, deletes, profile updates, unfollows, both ways. About a minute; needs
  `caddy`. Known GoToSocial issue: deleting a remote post that already has replies fails on its side,
  so that check only warns.
- `python3 test/interop/run-mastodon.py`: the same against a real **Mastodon** (4.7.3, `MASTODON_IMAGE`
  picks another) in Docker. Mastodon only federates with port 443, so everything lives in a private
  network namespace where Caddy serves `https://localhost` (Mastodon) and `https://127.0.0.1` (trough).
  Against servers reporting API version 7+ it also quotes both ways, waits for approval and revokes one.
  Needs docker and sudo.
- `python3 test/interop/run-akkoma.py`: a real **Akkoma** (`nixpkgs#akkoma` or `AKKOMA=/path`);
  `AKKOMA=$(nix build --no-link --print-out-paths nixpkgs#pleroma)` runs vanilla **Pleroma**. trough is
  `trough.localhost` there, because Erlang's TLS client rejects certificates for bare IPs.
- `python3 test/interop/run-misskey.py`: a real **Misskey** through its own API
  (`test/interop/interop_misskey.py`): follows, mentions, reactions, likes, renotes, quotes, replies,
  polls, followers-only and direct notes, deletes, profile updates. Misskey ignores remote edits (only
  warns). `MISSKEY_IMAGE=registry.activitypub.software/transfem-org/sharkey:latest` runs **Sharkey**.
- `LEMMY=/path/to/lemmy_server python3 test/interop/run-lemmy.py`: a real **Lemmy** (0.19.20) through its
  own API (`test/interop/interop_lemmy.py`): following a community, its posts in the home timeline,
  favourites as upvotes, replies as comments, Lemmy replies as mentions, edits, deletes, unfollowing.
  About five minutes, mostly waiting for trough to resend the first Follow (Lemmy loses the Accept for a
  server it has never seen). PostgreSQL runs in Docker, Lemmy natively in the namespace with fake DNS on
  `127.0.0.53` for `lemmy.localhost`/`trough.localhost`. Lemmy release builds only trust their built-in
  CA roots, so build one that honours `SSL_CERT_FILE` (~15 min, about 4 GB of RAM with thin LTO):
  ```sh
  git clone --recurse-submodules --branch 0.19.20 https://github.com/LemmyNet/lemmy && cd lemmy
  sed -i 's/"rustls-tls"/"rustls-tls-native-roots"/' Cargo.toml   # in the reqwest features
  # Rust >= 1.98 rejects the ambiguous glob import in crates/federate/src/util.rs: drop its
  # `use diesel::dsl::max;` line and call `diesel::dsl::max(id)` instead
  nix shell nixpkgs#{cargo,rustc,pkg-config,openssl,openssl.dev,postgresql.lib,postgresql.dev,gcc,protobuf} --command \
    env LIBRARY_PATH=$(nix eval --raw nixpkgs#postgresql.lib)/lib CARGO_PROFILE_RELEASE_LTO=thin \
    CARGO_PROFILE_RELEASE_CODEGEN_UNITS=16 cargo build --release -p lemmy_server
  ```

`AUTHORIZED_FETCH=true` runs Mastodon, Akkoma and Pleroma in secure mode (the runner first checks that
unsigned fetches are refused). Shared setup (namespace, containers, DNS, Caddy, trough) is in
`test/interop/interop_env.py`. `INTEROP_PAUSE=<seconds>` keeps a run up before cleanup (the temp
directory holds the Caddy and server logs; SIGTERM stops it early). Inside the namespace, use
`sudo nsenter --net=/proc/$(sudo docker inspect -f '{{.State.Pid}}' <tag>-net)/ns/net ...`.

### Client tests

- `python3 test/clients/mastodon_py.py <base-url> <image>` checks the Mastodon.py library against a dev
  instance with users alice and bob (see the script header).
- `nix shell nixpkgs#toot --command bash test/clients/toot.sh` runs the toot CLI against a disposable
  instance: login, posting, threads, polls, media, favourites, boosts, bookmarks, pins, follows, lists,
  followed tags, mutes, blocks, deletes.
- `bash test/clients/run-icecubes.sh` builds the iOS app Ice Cubes' `Models` and `NetworkClient`
  packages (pinned revision) with Swift on Linux plus shims for Apple-only APIs and drives a disposable
  instance through them like its screens do (`test/clients/icecubes/main.swift`).
- `bash test/clients/run-mastodon-ios.sh` does the same with the official Mastodon iOS app's entities
  and its own JSON decoder (`test/clients/mastodon-ios/main.swift`).

Both iOS tests use `test/clients/swift-shell.nix` (Swift from nixpkgs) and take a few minutes on the
first run. Phanpy, Elk and the Android app Tusky were checked by hand (browser and emulator).
