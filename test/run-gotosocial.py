#!/usr/bin/env python3
"""Federate a disposable trough with a disposable real GoToSocial and run test/gotosocial.py.

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
import socket
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.request

REPO = Path(__file__).resolve().parents[1]


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def gotosocial_binary():
    if os.environ.get("GOTOSOCIAL"):
        return Path(os.environ["GOTOSOCIAL"])
    out = subprocess.run(["bash", "-lc", "nix build --no-link --print-out-paths nixpkgs#gotosocial"],
                         capture_output=True, text=True, check=True).stdout.split()[-1]
    return Path(out) / "bin" / "gotosocial"


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=20)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def wait_for(url, ca, process, timeout=120, want=b""):
    ctx = ssl.create_default_context(cafile=str(ca)) if ca else None
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"{process.args[0]} exited with {process.returncode}")
        with contextlib.suppress(Exception):
            with urllib.request.urlopen(url, timeout=2, context=ctx) as r:
                if r.status == 200 and want in r.read():
                    return
        time.sleep(0.5)
    raise RuntimeError(f"{url} did not become ready")


def main():
    gts = gotosocial_binary()
    share = gts.parent.parent / "share" / "gotosocial" / "web"
    with tempfile.TemporaryDirectory(prefix="trough-gotosocial-") as temp, contextlib.ExitStack() as stack:
        root = Path(temp)
        env = {k: v for k, v in os.environ.items() if not k.startswith("TROUGH_")}
        gts_port, gts_tls, tr_port, tr_tls = (free_port() for _ in range(4))
        gts_host, tr_host = f"localhost:{gts_tls}", f"127.0.0.1:{tr_tls}"
        procs = []
        def start(name, args, **kw):
            log = stack.enter_context(open(root / f"{name}.log", "w+"))
            p = subprocess.Popen(args, stdout=log, stderr=subprocess.STDOUT, env=env, **kw)
            procs.append((name, p, log))
            return p
        try:
            # local TLS in front of both
            (root / "Caddyfile").write_text(f"""{{
	admin off
	local_certs
	skip_install_trust
	auto_https disable_redirects
	storage file_system {root}/caddy
}}
https://{gts_host} {{
	tls internal
	reverse_proxy 127.0.0.1:{gts_port}
}}
https://{tr_host} {{
	tls internal
	reverse_proxy 127.0.0.1:{tr_port}
}}
""")
            caddy = start("caddy", ["caddy", "run", "--config", str(root / "Caddyfile"), "--adapter", "caddyfile"])
            ca = root / "caddy/pki/authorities/local/root.crt"
            for _ in range(100):
                if ca.exists():
                    break
                time.sleep(0.2)
            env["SSL_CERT_FILE"] = str(ca)

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
            p = start("gotosocial", gts_cmd + ["server", "start"])
            wait_for(f"https://{gts_host}/api/v1/instance", ca, p, want=b'"uri"')
            stop(p)
            gal_pw = secrets.token_urlsafe(18) + "!A1"
            for args in (["admin", "account", "create", "--username", "gal", "--email", "gal@example.org", "--password", gal_pw],
                         ["admin", "account", "confirm", "--username", "gal"]):
                subprocess.run(gts_cmd + args, env=env, check=True, capture_output=True)
            p = start("gotosocial", gts_cmd + ["server", "start"])

            # trough
            binary = root / "trough"
            subprocess.run(["go", "build", "-o", str(binary), "."], cwd=REPO, check=True)
            flags = ["-insecure", "-scheme", "https", "-host", tr_host, "-data", str(root / "trough-data")]
            bob_pw = secrets.token_urlsafe(18)
            subprocess.run([str(binary), "user", "create", *flags, "-password-stdin", "bob"],
                           input=bob_pw + "\n", text=True, env=env, check=True, capture_output=True)
            tp = start("trough", [str(binary), "serve", *flags, "-listen", f"127.0.0.1:{tr_port}"])
            wait_for(f"https://{tr_host}/nodeinfo/2.0", ca, tp)
            wait_for(f"https://{gts_host}/api/v1/instance", ca, p, want=b'"uri"')

            result = subprocess.run([sys.executable, "-I", str(REPO / "test/gotosocial.py"),
                                     f"https://{tr_host}", "bob", bob_pw, f"https://{gts_host}", "gal@example.org", gal_pw], env=env)
            if result.returncode:
                for name, _, log in procs:
                    log.seek(0)
                    print(f"--- {name} log (tail) ---\n" + "".join(log.readlines()[-30:]))
            return result.returncode
        finally:
            for _, p, _ in reversed(procs):
                stop(p)


if __name__ == "__main__":
    if not shutil.which("caddy"):
        sys.exit("caddy is required on PATH")
    sys.exit(main())
