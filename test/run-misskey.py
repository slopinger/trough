#!/usr/bin/env python3
"""Federate a disposable trough with a disposable real Misskey and run test/interop_misskey.py.

Misskey (MISSKEY_IMAGE, default misskey/misskey:2026.10.0), PostgreSQL and
Redis run in Docker in a private loopback-only network namespace, like
test/run-mastodon.py; trough is https://trough.localhost there. Needs docker
and sudo, caddy and go. Containers and temp files are removed.

    python3 test/run-misskey.py
"""
import contextlib
import json
import os
from pathlib import Path
import secrets
import ssl
import subprocess
import sys
import tempfile
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parent))
from interop_env import REPO, Env, build, free_port, in_private_netns, pull, wait_for  # noqa: E402

MISSKEY = os.environ.get("MISSKEY_IMAGE", "misskey/misskey:2026.10.0")
POSTGRES = "postgres:16-alpine"
REDIS = "redis:7-alpine"


def main():
    tag = "trough-interop-" + secrets.token_hex(4)
    with tempfile.TemporaryDirectory(prefix="trough-misskey-") as temp:
        binary = Path(temp) / "trough"
        build(binary)
        pull(MISSKEY, POSTGRES, REDIS)
        return in_private_netns(tag, POSTGRES, __file__, str(binary))


def inside(tag, binary):
    with tempfile.TemporaryDirectory(prefix="trough-misskey-") as temp, contextlib.ExitStack() as stack:
        env = Env(temp, stack, {k: v for k, v in os.environ.items() if not k.startswith("TROUGH_")}, tag)
        root = env.root
        pg, rd, web, tp = (free_port() for _ in range(4))
        mhost, thost = "localhost", "trough.localhost"
        try:
            bundle = env.caddy({mhost: web, thost: tp})
            bundle.chmod(0o644)
            os.chmod(root, 0o755)
            (root / "hosts").write_text(f"127.0.0.1 localhost {thost}\n::1 localhost {thost}\n")
            config = root / "default.yml"
            config.write_text(json.dumps({  # JSON is YAML
                "url": f"https://{mhost}/", "port": web,
                "db": {"host": "127.0.0.1", "port": pg, "db": "misskey", "user": "misskey", "pass": ""},
                "redis": {"host": "127.0.0.1", "port": rd},
                "id": "aidx", "allowedPrivateNetworks": ["127.0.0.0/8", "::1/128"],
            }))
            config.chmod(0o644)

            env.postgres(POSTGRES, pg, "misskey")
            env.container("redis", REDIS, "redis-server", "--bind", "127.0.0.1", "--port", str(rd), "--save", "", "--appendonly", "no", detach=True)
            # Misskey asks DNS first and only reads /etc/hosts after an NXDOMAIN
            resolv = env.dns({mhost, thost})
            env.container("web", MISSKEY, detach=True, args=(
                "-v", f"{config}:/misskey/.config/default.yml:ro", "-v", f"{root}/hosts:/etc/hosts:ro",
                "-v", f"{resolv}:/etc/resolv.conf:ro",
                "-v", f"{bundle}:/ca.pem:ro", "-e", "NODE_EXTRA_CA_CERTS=/ca.pem"))

            tproc, bob_pw = env.trough(thost, tp, binary)
            wait_for(f"https://{thost}/nodeinfo/2.0", bundle, tproc)
            wait_for(f"https://{mhost}/nodeinfo/2.1", bundle, timeout=300, want=b"misskey")
            # the first account may be created without credentials, and becomes admin
            ctx = ssl.create_default_context(cafile=str(bundle))

            def api(endpoint, **data):
                req = urllib.request.Request(f"https://{mhost}/api/{endpoint}", headers={"Content-Type": "application/json"},
                                             data=json.dumps(data).encode())
                with urllib.request.urlopen(req, timeout=30, context=ctx) as r:
                    return json.loads(r.read() or b"null")
            token = api("admin/accounts/create", username="mia", password=secrets.token_urlsafe(18))["token"]
            api("admin/update-meta", i=token, federation="all")  # new instances start with federation off

            result = subprocess.run([sys.executable, "-I", str(REPO / "test/interop_misskey.py"),
                                     f"https://{thost}", "bob", bob_pw, f"https://{mhost}", token], env=env.env)
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
        sys.exit(inside(*sys.argv[2:4]))
    sys.exit(main())
