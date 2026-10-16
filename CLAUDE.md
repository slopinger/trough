# trough development notes

Small Go ActivityPub server with SQLite and a built-in server-rendered UI. Read README and docs/ for
the code map, features and deployment details. `AGENTS.md` points to this file.
Keep these notes, README and docs/ up to date whenever code behavior, commands or operations change.

- Layout (go.dev/doc/modules/layout): `cmd/trough` (CLI), `internal/server` (the App: `fed_*` federation,
  `api_*` Mastodon client API, `web_*` web UI, the rest core/storage), `internal/{ap,httpsig,ulid}`
  (leaf packages), `web/` (embedded templates/static), `test/interop` (real servers), `test/clients`
  (real clients), `docs/` (features, development with the code map, deployment). Keep docs/development.md's
  code map current when adding files.
- `go test ./...` and `go vet ./...` are baseline checks. Use `go test -race ./...` for concurrency changes.
- `fed_compat_*_test.go` is the ActivityPub compatibility suite: a live instance against a fake remote that
  speaks like Mastodon, GoToSocial, Misskey, Pleroma, Pixelfed, Friendica, WordPress, Fedify (RFC 9421),
  Lemmy and PeerTube, cross-checking signatures with go-fed/httpsig (imported as `gofed`). Add a
  flavor/fixture when fixing an interop bug. `ai_test.go` covers AI drafting against a fake server.
- Interop runs (docs/development.md has details): `python3 test/interop/run-federation.py` (two
  troughs), `run-gotosocial.py`, `run-mastodon.py`, `run-akkoma.py` (`AKKOMA=<pleroma path>`),
  `run-misskey.py` (`MISSKEY_IMAGE=...sharkey`), `run-lemmy.py` (`LEMMY=<lemmy_server>`, build in docs);
  `AUTHORIZED_FETCH=true` for secure mode, `INTEROP_PAUSE=<s>` to inspect a failed run. Shared setup is `test/interop/interop_env.py`.
- Client checks: `test/clients/` (Mastodon.py, `toot.sh`, `run-icecubes.sh`, `run-mastodon-ios.sh`).
  Keep entity shapes Mastodon-compatible (ids are strings, `Link` pagination, CORS). These are slow;
  run the relevant one when changing client API shapes, not for every change.
- Quote approval (FEP-044f, `quotes.go`): `TestQuoteHandshake` in `social_test.go` covers both directions;
  the interop scenario checks it against real Mastodon. trough reports Mastodon 4.5.0 / API version 7:
  only raise these when everything the Mastodon docs list for the new API version is implemented.
- ActivityPub C2S (`fed_c2s.go`, tests in `fed_c2s_test.go`) maps client activities onto the same actions as the
  UI/API; only the owner's bearer token may POST to an outbox or read an inbox.
- Relays, followed hashtags and lists: `extras_test.go`. Content from relays is only trusted after
  re-fetching it from its origin; only subscribed relays may forward other actors' activities.
- AI drafting (`ai.go`) calls DeepSeek (OpenAI-compatible) with the key from `TROUGH_DEEPSEEK_API_KEY`
  (production: root-only `/etc/trough-secrets.env`) or the admin setting. Drafts never post themselves;
  keep remote text delimited as data in prompts.
- Parser and media cache repair/cleanup regression/fuzz tests are in `media_test.go`; thread cycle/depth tests in `timeline_test.go`;
  redirect/signature/domain-policy tests in `fed_fetch_test.go`; incoming Undo tests in `fed_inbox_test.go`.
- `go test ./internal/server -run '^$' -fuzz '^FuzzJPEGOrientation$' -fuzztime=30s` fuzzes JPEG metadata.
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
  applies. Verify package builds and relevant flake checks; document supported commands in docs/deployment.md.

- Nix: `nix build`, `nix flake check`, `nix develop`; commit `flake.lock`.
  `nixosModules.default` exports the reusable host module; see docs/deployment.md for options and limits.
- Never add Co-Authored-By trailers or session links to public GitLab content.
