#!/usr/bin/env python3
"""Federate a disposable trough with a disposable real GoToSocial and run test/interop.py.

GoToSocial always uses https for WebFinger, so both servers sit behind a
throwaway Caddy with its own local CA on loopback; trough runs with
-insecure -scheme https. Needs `caddy` on PATH and a gotosocial binary:
GOTOSOCIAL=/path/to/gotosocial, or it is built with Nix (nixpkgs#gotosocial).
Everything lives in a temporary directory that is removed afterwards.
"""
import contextlib
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import tempfile

sys.path.insert(0, str(Path(__file__).resolve().parent))
from interop_env import REPO, Env, free_port, stop, wait_for  # noqa: E402


def gotosocial_binary():
    if os.environ.get("GOTOSOCIAL"):
        return Path(os.environ["GOTOSOCIAL"])
    out = subprocess.run(["bash", "-lc", "nix build --no-link --print-out-paths nixpkgs#gotosocial"],
                         capture_output=True, text=True, check=True).stdout.split()[-1]
    return Path(out) / "bin" / "gotosocial"


def main():
    gts = gotosocial_binary()
    share = gts.parent.parent / "share" / "gotosocial" / "web"
    with tempfile.TemporaryDirectory(prefix="trough-gotosocial-") as temp, contextlib.ExitStack() as stack:
        env = Env(temp, stack, {k: v for k, v in os.environ.items() if not k.startswith("TROUGH_")})
        root = env.root
        gts_port, gts_tls, tr_port, tr_tls = (free_port() for _ in range(4))
        gts_host, tr_host = f"localhost:{gts_tls}", f"127.0.0.1:{tr_tls}"
        try:
            ca = env.caddy({gts_host: gts_port, tr_host: tr_port})

            # GoToSocial: migrate on first start, then create the user offline
            (root / "gts").mkdir()
            cfg = root / "gts/config.yaml"
            cfg.write_text(json.dumps({
                "host": gts_host, "protocol": "https", "bind-address": "127.0.0.1", "port": gts_port,
                "db-type": "sqlite", "db-address": str(root / "gts/gts.db"),
                "storage-local-base-path": str(root / "gts/storage"),
                "web-template-base-dir": str(share / "template") + "/", "web-asset-base-dir": str(share / "assets") + "/",
                "http-client": {"allow-ips": ["127.0.0.0/8"]}, "log-level": "info",
                "cache": {"memory-target": "100MiB"},
            }))
            gts_cmd = [str(gts), "--config-path", str(cfg)]
            p = env.start("gotosocial", gts_cmd + ["server", "start"])
            wait_for(f"https://{gts_host}/api/v1/instance", ca, p, want=b'"uri"')
            stop(p)
            gal_pw = secrets.token_urlsafe(18) + "!A1"
            for args in (["admin", "account", "create", "--username", "gal", "--email", "gal@example.org", "--password", gal_pw],
                         ["admin", "account", "confirm", "--username", "gal"]):
                subprocess.run(gts_cmd + args, env=env.env, check=True, capture_output=True)
            p = env.start("gotosocial", gts_cmd + ["server", "start"])

            tp, bob_pw = env.trough(tr_host, tr_port)
            wait_for(f"https://{tr_host}/nodeinfo/2.0", ca, tp)
            wait_for(f"https://{gts_host}/api/v1/instance", ca, p, want=b'"uri"')

            result = subprocess.run([sys.executable, "-I", str(REPO / "test/interop.py"),
                                     f"https://{tr_host}", "bob", bob_pw, f"https://{gts_host}", "gal@example.org", gal_pw], env=env.env)
            if result.returncode:
                env.dump_logs()
            return result.returncode
        finally:
            env.close()


if __name__ == "__main__":
    if not shutil.which("caddy"):
        sys.exit("caddy is required on PATH")
    sys.exit(main())
