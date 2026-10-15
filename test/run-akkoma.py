#!/usr/bin/env python3
"""Federate a disposable trough with a disposable real Akkoma or Pleroma and run test/interop.py.

Akkoma (the Pleroma fork, built with Nix: nixpkgs#akkoma, or AKKOMA=/path to
a release, which can also be Pleroma's) and PostgreSQL run in a private loopback-only network namespace,
like test/run-mastodon.py. Akkoma runs in a container that mounts /nix/store
read-only, because its HTTP client only trusts the OS certificate store and
the container lets the test CA stand in for it. Needs docker and sudo, caddy,
go and nix. Containers and temp files are removed.

    python3 test/run-akkoma.py
    AUTHORIZED_FETCH=true python3 test/run-akkoma.py
    AKKOMA=$(nix build --no-link --print-out-paths nixpkgs#pleroma) python3 test/run-akkoma.py
"""
import contextlib
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile

sys.path.insert(0, str(Path(__file__).resolve().parent))
from interop_env import REPO, Env, build, free_port, in_private_netns, pull, require_signed_fetches, wait_for  # noqa: E402

POSTGRES = "postgres:16-alpine"


def akkoma_release():
    if os.environ.get("AKKOMA"):
        return Path(os.environ["AKKOMA"])
    out = subprocess.run(["bash", "-lc", "nix build --no-link --print-out-paths nixpkgs#akkoma"],
                         capture_output=True, text=True, check=True).stdout.split()[-1]
    return Path(out)


def main():
    tag = "trough-interop-" + secrets.token_hex(4)
    with tempfile.TemporaryDirectory(prefix="trough-akkoma-") as temp:
        binary = Path(temp) / "trough"
        build(binary)
        release = akkoma_release()
        pull(POSTGRES)
        return in_private_netns(tag, POSTGRES, __file__, str(binary), str(release))


def inside(tag, binary, release):
    with tempfile.TemporaryDirectory(prefix="trough-akkoma-") as temp, contextlib.ExitStack() as stack:
        env = Env(temp, stack, {k: v for k, v in os.environ.items() if not k.startswith("TROUGH_")}, tag)
        root = env.root
        pg, web, tp = (free_port() for _ in range(3))
        # trough needs a DNS name: Erlang's TLS client rejects the test CA's
        # certificates for bare IPs (their critical subjectAltName goes unchecked)
        ahost, thost = "localhost", "trough.localhost"
        try:
            bundle = env.caddy({ahost: web, thost: tp})
            bundle.chmod(0o644)
            os.chmod(root, 0o755)
            data = root / "akkoma"
            for d in ("uploads", "static", "tmp", "home"):
                (data / d).mkdir(parents=True)
            (data / "hosts").write_text(f"127.0.0.1 localhost {thost}\n::1 localhost {thost}\n")
            authorized = os.environ.get("AUTHORIZED_FETCH") == "true"
            # Pleroma's Hackney trusts certifi's bundle, not the OS store, unless told otherwise
            hackney = ""
            if any(b"Tesla.Adapter.Hackney" in f.read_bytes() for f in Path(release).glob("releases/*/sys.config")):
                hackney = ("config :pleroma, :http, adapter: [ssl_options: [verify: :verify_peer, "
                           "cacertfile: \"/etc/ssl/certs/ca-certificates.crt\", "
                           "customize_hostname_check: [match_fun: :public_key.pkix_verify_hostname_match_fun(:https)]]]\n")
            (data / "config.exs").write_text(f"""import Config

config :pleroma, Pleroma.Web.Endpoint,
  url: [host: "{ahost}", scheme: "https", port: 443],
  http: [ip: {{127, 0, 0, 1}}, port: {web}],
  secret_key_base: "{secrets.token_urlsafe(48)}",
  signing_salt: "{secrets.token_urlsafe(6)}",
  live_view: [signing_salt: "{secrets.token_urlsafe(6)}"]

config :pleroma, :instance,
  name: "akkoma interop", email: "admin@{ahost}", notify_email: "admin@{ahost}",
  registrations_open: false, federating: true, static_dir: "{data}/static"

config :pleroma, :activitypub, authorized_fetch_mode: {str(authorized).lower()}
config :pleroma, :media_proxy, enabled: false
config :pleroma, :frontends, primary: %{{"name" => "none", "ref" => "none"}}
config :pleroma, Pleroma.Repo,
  adapter: Ecto.Adapters.Postgres, username: "akkoma", password: "", database: "akkoma",
  hostname: "127.0.0.1", port: {pg}
config :pleroma, :database, rum_enabled: false
config :pleroma, configurable_from_database: false
config :pleroma, Pleroma.Upload, uploader: Pleroma.Uploaders.Local, filters: [], base_url: "https://{ahost}/media/"
config :pleroma, Pleroma.Uploaders.Local, uploads: "{data}/uploads"
config :tzdata, :autoupdate, :disabled
{hackney}config :joken, default_signer: "{secrets.token_urlsafe(48)}"
""")
            (data / "config.exs").chmod(0o600)  # Akkoma refuses a world-readable config

            def akkoma(name, *cmd, detach=False):
                # the Nix-built release, with the test CA as the OS trust store
                return env.container(name, POSTGRES, *cmd, detach=detach, args=(
                    "--user", f"{os.getuid()}:{os.getgid()}", "--entrypoint", f"{release}/bin/pleroma_ctl" if cmd[0] != "start" else f"{release}/bin/pleroma",
                    "-v", "/nix/store:/nix/store:ro", "-v", f"{root}:{root}",
                    "-v", f"{bundle}:/etc/ssl/certs/ca-certificates.crt:ro", "-v", f"{data}/hosts:/etc/hosts:ro",
                    "-e", f"AKKOMA_CONFIG_PATH={data}/config.exs", "-e", f"PLEROMA_CONFIG_PATH={data}/config.exs",
                    "-e", f"HOME={data}/home", "-e", f"RELEASE_TMP={data}/tmp", "-e", "RELEASE_COOKIE=interop",
                    "-e", "RELEASE_DISTRIBUTION=none", "-e", "PLEROMA_CTL_RPC_DISABLED=true",
                    "-e", "LANG=C.UTF-8", "-e", "ELIXIR_ERL_OPTIONS=+fnu", "-w", str(data),
                ))

            env.postgres(POSTGRES, pg, "akkoma")
            subprocess.run(["sudo", "docker", "exec", f"{tag}-pg", "psql", "-h", "127.0.0.1", "-p", str(pg), "-U", "akkoma", "-c",
                            "CREATE EXTENSION IF NOT EXISTS citext; CREATE EXTENSION IF NOT EXISTS pg_trgm;"
                            ' CREATE EXTENSION IF NOT EXISTS "uuid-ossp";'], check=True, capture_output=True)
            akkoma("migrate", "migrate")
            mal_pw = secrets.token_urlsafe(18)
            akkoma("user", "user", "new", "mal", "mal@localhost", "--password", mal_pw, "--assume-yes")
            akkoma("web", "start", detach=True)

            tproc, bob_pw = env.trough(thost, tp, binary)
            wait_for(f"https://{thost}/nodeinfo/2.0", bundle, tproc)
            wait_for(f"https://{ahost}/api/v1/instance", bundle, timeout=180, want=b'"uri"')
            if authorized:
                # Akkoma keeps actors public so their keys can be fetched
                require_signed_fetches(f"https://{ahost}/users/mal/outbox", bundle)

            result = subprocess.run([sys.executable, "-I", str(REPO / "test/interop.py"),
                                     f"https://{thost}", "bob", bob_pw, f"https://{ahost}", "--password", "mal", mal_pw], env=env.env)
            if result.returncode:
                env.dump_logs(200)
            return result.returncode
        except Exception:
            env.dump_logs(200)
            raise
        finally:
            env.close()


if __name__ == "__main__":
    if sys.argv[1:2] == ["--inside"]:
        sys.exit(inside(*sys.argv[2:5]))
    sys.exit(main())
