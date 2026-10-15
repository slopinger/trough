# trough development notes

Small Go ActivityPub server with SQLite and a built-in server-rendered UI. Read README for the
code map, federation features/limits and deployment details. `AGENTS.md` points to this file.
Keep these notes and README up to date whenever code behavior, commands or operations change.

- `go test ./...` and `go vet ./...` are baseline checks. Use `go test -race ./...` for concurrency changes.
- `python3 test/run-federation.py` builds two disposable loopback instances and exercises signed
  federation via the real web UI; it removes processes/data on exit. It never uses live credentials.
- `python3 test/run-gotosocial.py` federates with a disposable real GoToSocial (Nix) behind a throwaway
  local-CA Caddy (trough runs `-insecure -scheme https`); needs `caddy`, takes ~1 minute.
  `python3 test/run-mastodon.py` (optionally `AUTHORIZED_FETCH=true`) does the same with a real Mastodon
  in Docker, inside a private loopback-only network namespace so Caddy can use port 443;
  `python3 test/run-akkoma.py` likewise with a real Akkoma (Nix; `AKKOMA=<nixpkgs#pleroma path>` for Pleroma). These drive `test/interop.py`;
  `python3 test/run-misskey.py` drives a real Misskey through `test/interop_misskey.py` (its own API;
  `MISSKEY_IMAGE=registry.activitypub.software/transfem-org/sharkey:latest` runs it against Sharkey).
  Shared setup (netns, containers, DNS, Caddy, trough) is in `test/interop_env.py`;
  `INTEROP_PAUSE=<s>` keeps a failed run up for inspection.
- `compat_*_test.go` is the ActivityPub compatibility suite: a live instance against a fake remote that
  speaks like Mastodon, GoToSocial, Misskey, Pleroma, Pixelfed, Friendica, WordPress, Fedify (RFC 9421),
  Lemmy and PeerTube, cross-checking signatures with go-fed/httpsig. Add a flavor/fixture when fixing an
  interop bug. `ai_test.go` covers AI drafting against a fake streaming chat completions server.
- Mastodon client API (`api_*.go`): `api_test.go` drives OAuth (PKCE), posting, timelines, media,
  notifications and WebSocket streaming; `test/mastodon_py.py` checks a real client library against a dev
  instance; `nix shell nixpkgs#toot --command bash test/toot.sh` runs the toot CLI against a disposable one;
  `bash test/run-icecubes.sh` builds the iOS app Ice Cubes' Models/NetworkClient (pinned revision) with
  Swift on Linux and drives a disposable instance with them (`test/icecubes/main.swift`, ~5 min first run). Keep entity shapes Mastodon-compatible (ids are strings, `Link` pagination, CORS).
- Quote approval (FEP-044f, `quotes.go`): `TestQuoteHandshake` in `social_test.go` covers both directions;
  the interop scenario checks it against real Mastodon. trough reports Mastodon 4.5.0 / API version 7:
  only raise these when everything the Mastodon docs list for the new API version is implemented.
- ActivityPub C2S (`c2s.go`, tests in `c2s_test.go`) maps client activities onto the same actions as the
  UI/API; only the owner's bearer token may POST to an outbox or read an inbox.
- Relays, followed hashtags and lists: `extras_test.go`. Content from relays is only trusted after
  re-fetching it from its origin; only subscribed relays may forward other actors' activities.
- AI drafting (`ai.go`) calls DeepSeek (OpenAI-compatible) with the key from `TROUGH_DEEPSEEK_API_KEY`
  (production: root-only `/etc/trough-secrets.env`) or the admin setting. Drafts never post themselves;
  keep remote text delimited as data in prompts.
- Parser and media cache repair/cleanup regression/fuzz tests are in `media_test.go`; thread cycle/depth tests in `timeline_test.go`;
  redirect/signature/domain-policy tests in `fetch_test.go`; incoming Undo tests in `inbox_test.go`.
- `go test -run '^$' -fuzz '^FuzzJPEGOrientation$' -fuzztime=30s` fuzzes JPEG metadata.
- Keep remote content untrusted. Preserve visibility checks, HTTP signature/actor ownership checks,
  SSRF protections, domain blocks across redirects, upload bounds and sanitization.
- `./deploy.sh` tests/builds and restarts the live trough service. Check git status before edits or
  commits; use a worktree for concurrent unrelated work. Sign and push only your own changes.
- Production: `fedi.freifick.net`, loopback `127.0.0.1:8790`, service user `trough`, `/etc/trough.env`,
  `/var/lib/trough/{trough.sqlite3,media,cache}`. The hostname is embedded in federation IDs and cannot change.
  Inspect live SQLite as `trough`, preserving WAL ownership. Never commit data, private keys or passwords.
- Daily verified local backups come from the separate `slop/server-backups` project/timer, not this
  deploy script. Cache is disposable; database and uploaded media are not. Restore instructions are there.
- Use small commits: one logical change per commit (with its tests/docs), each passing the checks above.
  Don't bundle unrelated changes or a whole session's work into one commit.

- Every software change must maintain project-owned Nix flake packaging, a committed lockfile,
  development tools and checks. Export a reusable NixOS module where host/service integration
  applies. Verify package builds and relevant flake checks; document supported commands in README.

- Nix: `nix build`, `nix flake check`, `nix develop`; commit `flake.lock`.
  `nixosModules.default` exports the reusable host module; see README for options and limits.
- Never add Co-Authored-By trailers or session links to public GitLab content.
