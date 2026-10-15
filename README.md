# trough

A small, single-binary **fediverse (ActivityPub) server** with a built-in web interface.
A trough is where the slop goes; this one feeds you posts from the fediverse.

It is modelled on [GoToSocial](https://gotosocial.org/): one Go binary, SQLite, low resource use,
federating with Mastodon, GoToSocial, Misskey, Pleroma/Akkoma and friends. Like GoToSocial it speaks the
Mastodon client API, so apps such as Tusky, Ivory, Elk, Phanpy, Ice Cubes or Fedilab work with it, and it
also ships its own server-rendered web interface that works without JavaScript (a little JS only adds
conveniences).

Running instance: **https://fedi.freifick.net**

## Features

- **Federation** with Mastodon, GoToSocial, Misskey/Sharkey, Pleroma/Akkoma, Lemmy, PeerTube and
  others: HTTP Signatures (draft-cavage and RFC 9421), authorized-fetch mode, a persistent delivery
  queue, relays, ActivityPub client-to-server, quote posts with Mastodon 4.5's approval handshake
- **Mastodon client API** (reports Mastodon 4.5.0, API version 7): OAuth with PKCE, streaming,
  Web Push, grouped notifications, filters, lists, quotes, scheduled posts
- **Web interface** without required JavaScript: timelines, threads, compose with polls, media,
  content warnings and AI drafting (optional), profiles, notifications, search, settings, admin page
- Media re-encoding (EXIF stripped) and a caching media proxy; strict CSP, CSRF tokens, rate limits

The full list is in [docs/features.md](docs/features.md).

## Quick start

```sh
go build -o trough ./cmd/trough
./trough user create -host example.org -data ./data -admin alice   # prints a password
./trough serve -host example.org -listen 127.0.0.1:8790 -data ./data
```

Put a TLS-terminating reverse proxy in front of it. `-insecure` runs it on plain HTTP for local tests.

## Documentation

- [docs/features.md](docs/features.md): what trough does, in detail
- [docs/development.md](docs/development.md): repository layout, code map, local runs and tests
- [docs/deployment.md](docs/deployment.md): running it, fedi.freifick.net, backups, Nix package and module

## License

MIT
