# Deployment

## Running trough

```sh
go build -o trough ./cmd/trough

# create an account (prints a random password)
./trough user create -host example.org -data ./data -admin alice

# run (behind a TLS-terminating reverse proxy that forwards to 127.0.0.1:8790)
./trough serve -host example.org -listen 127.0.0.1:8790 -data ./data
```

Other commands: `trough user password <name>`, `trough user admin <name> on|off`, `trough user suspend <name> on|off`, `trough user list`,
`trough fetch <url>` (signed GET of an ActivityPub object, for debugging). Every flag can also be set as
an environment variable (`TROUGH_HOST`, `TROUGH_LISTEN`, `TROUGH_DATA`, ...).

The host name is baked into every ID that is handed out, so it can't be changed later. The database
remembers its host and refuses to start with a different one.

The data directory holds `trough.sqlite3`, uploaded `media/` and the media proxy's `cache/`. Back up the
database and `media/`; `cache/` can be dropped. Never copy a live SQLite file without its WAL or the
online backup API.

## fedi.freifick.net

`./deploy.sh` runs the tests, builds a static binary, installs it to `/usr/local/bin/trough` along with
`troughctl` (runs admin commands as the service user), the hardened `trough.service` (user `trough`, data in
`/var/lib/trough`, config in `/etc/trough.env`) and the Caddy site config from `deploy/`, then restarts
the service. The AI key lives in the root-only `/etc/trough-secrets.env`, which `trough.service` loads.

```sh
troughctl user create someone          # new account, prints the password
troughctl user list
journalctl -u trough -f                # logs
```

[server-backups](https://git.freifick.net/slop/server-backups) takes daily verified online snapshots
with `slop-services-backup.timer`, retaining 14 archives in root-only `/srv/backups/slop-services/`.
That repo documents manual verification and restore. The backups are local/same-disk; no off-site
copy is configured.

## Nix

`nix build` packages the binary (`cmd/trough`) with Go 1.27 and runs the Go test suite in the sandbox.
`nix flake check` builds that package check. `nix develop` provides Go 1.27, Python, Caddy and SQLite.
Linux x86_64/aarch64 are defined; x86_64 was built and tested. `nix run . -- --help` runs the CLI.
After changing go.mod/go.sum, update the flake `vendorHash` to the hash reported by a build with
`vendorHash = pkgs.lib.fakeHash`, then rebuild.

Import `inputs.trough.nixosModules.default` and set `services.trough.enable` and
`services.trough.host = "fedi.example.org";`. Options `package`, `listen` (default loopback 8790) and
`environmentFile` configure the runtime. Secrets must be in a runtime file, never a store path or Nix
literal. The module creates the trough user and `/var/lib/trough`; arrange TLS reverse proxying and
backups separately. The existing Arch deployment continues through `./deploy.sh`.
