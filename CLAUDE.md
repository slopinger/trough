# trough development notes

Small Go ActivityPub server with SQLite and a built-in server-rendered UI. Read README for the
code map, federation features/limits and deployment details. `AGENTS.md` points to this file.
Keep these notes and README up to date whenever code behavior, commands or operations change.

- `go test ./...` and `go vet ./...` are baseline checks. Use `go test -race ./...` for concurrency changes.
- `python3 test/run-federation.py` builds two disposable loopback instances and exercises signed
  federation via the real web UI; it removes processes/data on exit. It never uses live credentials.
- `compat_*_test.go` is the ActivityPub compatibility suite: a live instance against a fake remote that
  speaks like Mastodon, GoToSocial, Misskey, Pleroma, Pixelfed, Friendica, WordPress, Fedify (RFC 9421),
  Lemmy and PeerTube, cross-checking signatures with go-fed/httpsig. Add a flavor/fixture when fixing an
  interop bug. `ai_test.go` covers AI drafting against a fake streaming chat completions server.
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
