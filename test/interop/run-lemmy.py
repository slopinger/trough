#!/usr/bin/env python3
"""Federate a disposable trough with a disposable real Lemmy and run test/interop/interop_lemmy.py.

Lemmy's HTTP client only trusts the root certificates compiled into it, and
release builds refuse plain http, so this needs a Lemmy server built with
reqwest's "rustls-tls-native-roots" feature (which honours SSL_CERT_FILE):
LEMMY=/path/to/lemmy_server (see docs/development.md). It runs with
PostgreSQL (Docker) in a private loopback-only network namespace like
test/interop/run-mastodon.py, as https://lemmy.localhost next to
https://trough.localhost (Lemmy refuses a bare "localhost"). Needs docker,
sudo, caddy and go. Containers and temp files are removed.

    LEMMY=.../target/release/lemmy_server python3 test/interop/run-lemmy.py
"""
import contextlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile

sys.path.insert(0, str(Path(__file__).resolve().parent))
from interop_env import REPO, Env, build, free_port, in_private_netns, pull, wait_for  # noqa: E402

POSTGRES = "postgres:16-alpine"


def main():
    lemmy = os.environ.get("LEMMY")
    if not lemmy or not os.access(lemmy, os.X_OK):
        sys.exit("set LEMMY to a lemmy_server binary built with native root certificates")
    tag = "trough-interop-" + secrets.token_hex(4)
    with tempfile.TemporaryDirectory(prefix="trough-lemmy-") as temp:
        binary = Path(temp) / "trough"
        build(binary)
        pull(POSTGRES)
        return in_private_netns(tag, POSTGRES, __file__, str(binary), str(Path(lemmy).resolve()))


def inside(tag, binary, lemmy):
    with tempfile.TemporaryDirectory(prefix="trough-lemmy-") as temp, contextlib.ExitStack() as stack:
        env = Env(temp, stack, {k: v for k, v in os.environ.items() if not k.startswith("TROUGH_")}, tag)
        root = env.root
        pg, lp, tp = (free_port() for _ in range(3))
        lhost, thost = "lemmy.localhost", "trough.localhost"
        try:
            bundle = env.caddy({lhost: lp, thost: tp})
            # Lemmy (a Nix build: its glibc can't load the host's nss-resolve)
            # asks the nameserver in the host's resolv.conf, the resolved stub
            env.dns({lhost, thost}, addr="127.0.0.53")
            env.postgres(POSTGRES, pg, "lemmy")
            admin_pw = secrets.token_urlsafe(18)
            config = root / "lemmy.hjson"
            config.write_text(json.dumps({  # JSON is Hjson
                "hostname": lhost, "bind": "127.0.0.1", "port": lp, "tls_enabled": True,
                "database": {"uri": f"postgresql://lemmy@127.0.0.1:{pg}/lemmy"},
                "pictrs": {"url": "http://127.0.0.1:9/", "image_mode": "None"},
                "setup": {"admin_username": "lemmy_admin", "admin_password": admin_pw, "site_name": "lemmy interop"},
            }))
            env.env.update({"LEMMY_CONFIG_LOCATION": str(config), "RUST_LOG": os.environ.get("RUST_LOG", "warn"), "SSL_CERT_FILE": str(bundle),
                            "DANGER_FEDERATION_ALLOW_LOCAL_IP": "1",  # everything is on loopback here
                            "LEMMY_TEST_FAST_FEDERATION": "1"})  # send queued activities right away, not every ~30s
            env.start("lemmy", [lemmy])
            tproc, bob_pw = env.trough(thost, tp, binary)
            wait_for(f"https://{thost}/nodeinfo/2.0", bundle, tproc)
            wait_for(f"https://{lhost}/api/v3/site", bundle, timeout=180, want=b'"site_view"')
            result = subprocess.run([sys.executable, "-I", str(REPO / "test/interop/interop_lemmy.py"),
                                     f"https://{thost}", "bob", bob_pw, f"https://{lhost}", "lemmy_admin", admin_pw], env=env.env)
            if result.returncode:
                env.dump_logs(60)
            return result.returncode
        except Exception:
            env.dump_logs(60)
            raise
        finally:
            env.close()


if __name__ == "__main__":
    if sys.argv[1:2] == ["--inside"]:
        sys.exit(inside(*sys.argv[2:5]))
    sys.exit(main())
