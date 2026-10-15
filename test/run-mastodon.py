#!/usr/bin/env python3
"""Federate a disposable trough with a disposable real Mastodon and run test/interop.py.

Mastodon only federates with servers on the default https port, so everything
runs in a private network namespace of its own: a `--network none` Docker
container holds it, Mastodon (web + Sidekiq), PostgreSQL and Redis join it,
and this script re-runs itself inside it (nsenter) to start a local-CA Caddy on
port 443 for https://localhost (Mastodon) and https://127.0.0.1 (trough). The
namespace has only a loopback interface, so nothing is reachable from outside.
Needs docker and sudo, caddy and go. Containers and temp files are removed.

    python3 test/run-mastodon.py              # MASTODON_IMAGE=... to pick a version
    AUTHORIZED_FETCH=true python3 test/run-mastodon.py
"""
import contextlib
import os
from pathlib import Path
import secrets
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parent))
from interop_env import REPO, Env, build, free_port, wait_for  # noqa: E402

MASTODON = os.environ.get("MASTODON_IMAGE", "ghcr.io/mastodon/mastodon:v4.7.3")
POSTGRES = "postgres:16-alpine"
REDIS = "redis:7-alpine"


def docker(*args):
    r = subprocess.run(["sudo", "docker", *args], text=True, capture_output=True)
    if r.returncode:
        raise RuntimeError(f"docker {' '.join(args[-4:])} failed:\n{r.stdout[-2000:]}{r.stderr[-3000:]}")
    return r


def main():
    """Outside: create the namespace and run inside() in it."""
    tag = "trough-interop-" + secrets.token_hex(4)
    with tempfile.TemporaryDirectory(prefix="trough-mastodon-") as temp:
        binary = Path(temp) / "trough"
        build(binary)  # here, where the module proxy is reachable
        for image in (MASTODON, POSTGRES, REDIS):
            if subprocess.run(["sudo", "docker", "image", "inspect", image], capture_output=True).returncode:
                docker("pull", image)
        docker("run", "-d", "--rm", "--network", "none", "--name", f"{tag}-net", POSTGRES, "sleep", "infinity")
        try:
            pid = docker("inspect", "-f", "{{.State.Pid}}", f"{tag}-net").stdout.strip()
            return subprocess.run([
                "sudo", "-E", "nsenter", f"--net=/proc/{pid}/ns/net",
                "setpriv", f"--reuid={os.getuid()}", f"--regid={os.getgid()}", "--init-groups",
                "env", f"PATH={os.environ['PATH']}",
                sys.executable, "-I", __file__, "--inside", tag, str(binary),
            ]).returncode
        finally:
            docker("rm", "-f", f"{tag}-net")


def inside(tag, binary):
    containers = []
    with tempfile.TemporaryDirectory(prefix="trough-mastodon-") as temp, contextlib.ExitStack() as stack:
        env = Env(temp, stack, {k: v for k, v in os.environ.items() if not k.startswith("TROUGH_")})
        root = env.root
        pg, rd, web, tp = (free_port() for _ in range(4))
        mhost, thost = "localhost", "127.0.0.1"
        try:
            bundle = env.caddy({mhost: web, thost: tp})
            bundle.chmod(0o644)
            os.chmod(root, 0o755)
            menv = root / "mastodon.env"

            def run(name, image, *cmd, detach=False, extra=()):
                args = ["run", "--rm", "--network", f"container:{tag}-net", *extra]
                if detach:
                    args += ["-d", "--name", f"{tag}-{name}"]
                    containers.append(f"{tag}-{name}")
                if image == MASTODON:
                    args += ["--env-file", str(menv), "-v", f"{bundle}:/ca.pem:ro"]
                return docker(*args, image, *cmd)

            # trust auth is fine: nothing outside this namespace can reach postgres
            run("pg", POSTGRES, "-c", "listen_addresses=127.0.0.1", "-p", str(pg), detach=True,
                extra=("-e", "POSTGRES_HOST_AUTH_METHOD=trust", "-e", "POSTGRES_USER=mastodon"))
            run("redis", REDIS, "redis-server", "--bind", "127.0.0.1", "--port", str(rd), "--save", "", "--appendonly", "no", detach=True)
            hexkey = lambda: secrets.token_hex(64)
            menv.write_text("\n".join([
                f"LOCAL_DOMAIN={mhost}", "RAILS_ENV=production", "NODE_ENV=production",
                "DB_HOST=127.0.0.1", f"DB_PORT={pg}", "DB_USER=mastodon", "DB_NAME=mastodon", "DB_PASS=",
                "REDIS_HOST=127.0.0.1", f"REDIS_PORT={rd}",
                f"SECRET_KEY_BASE={hexkey()}", f"OTP_SECRET={hexkey()}",
                f"ACTIVE_RECORD_ENCRYPTION_DETERMINISTIC_KEY={secrets.token_hex(16)}",
                f"ACTIVE_RECORD_ENCRYPTION_KEY_DERIVATION_SALT={secrets.token_hex(16)}",
                f"ACTIVE_RECORD_ENCRYPTION_PRIMARY_KEY={secrets.token_hex(16)}",
                "ALLOWED_PRIVATE_ADDRESSES=127.0.0.0/8", "SSL_CERT_FILE=/ca.pem",
                "BIND=127.0.0.1", f"PORT={web}", "RAILS_SERVE_STATIC_FILES=true", "ES_ENABLED=false",
                "SAFETY_ASSURED=1", "DISABLE_DATABASE_ENVIRONMENT_CHECK=1", "SIDEKIQ_CONCURRENCY=5",
                f"AUTHORIZED_FETCH={os.environ.get('AUTHORIZED_FETCH', 'false')}",
            ]) + "\n")
            for _ in range(60):
                if subprocess.run(["sudo", "docker", "exec", f"{tag}-pg", "pg_isready", "-h", "127.0.0.1", "-p", str(pg)],
                                  capture_output=True).returncode == 0:
                    break
                time.sleep(1)
            else:
                raise RuntimeError("postgres did not start")
            run("setup", MASTODON, "bundle", "exec", "rails", "db:setup")
            # create the user directly: tootctl insists on a reachable mail domain,
            # and Mastodon resets `approved` on create
            ruby = ("u = User.new(email: 'mal@localhost', password: SecureRandom.hex(16), agreement: true,"
                    " account: Account.new(username: 'mal')); u.save!(validate: false);"
                    "u.update_columns(approved: true, confirmed_at: Time.now);"
                    "a = Doorkeeper::Application.create!(name: 'interop', redirect_uri: 'urn:ietf:wg:oauth:2.0:oob', scopes: 'read write follow push');"
                    "t = Doorkeeper::AccessToken.create!(application_id: a.id, resource_owner_id: u.id, scopes: 'read write follow push');"
                    "puts 'TOKEN=' + t.token")
            token = run("user", MASTODON, "bin/rails", "runner", ruby).stdout.split("TOKEN=", 1)[1].split()[0]
            run("web", MASTODON, "bundle", "exec", "puma", "-C", "config/puma.rb", detach=True)
            run("sidekiq", MASTODON, "bundle", "exec", "sidekiq", detach=True)

            tproc, bob_pw = env.trough(thost, tp, binary)
            wait_for(f"https://{thost}/nodeinfo/2.0", bundle, tproc)
            wait_for(f"https://{mhost}/api/v1/instance", bundle, timeout=240, want=b'"uri"')
            if os.environ.get("AUTHORIZED_FETCH") == "true":  # make sure secure mode is really on
                req = urllib.request.Request(f"https://{mhost}/users/mal", headers={"Accept": "application/activity+json"})
                try:
                    urllib.request.urlopen(req, context=ssl.create_default_context(cafile=str(bundle)))
                    raise RuntimeError("AUTHORIZED_FETCH is set but Mastodon served an unsigned fetch")
                except urllib.error.HTTPError as e:
                    assert e.code == 401, e.code
                print("Mastodon requires signed fetches (AUTHORIZED_FETCH)")

            result = subprocess.run([sys.executable, "-I", str(REPO / "test/interop.py"),
                                     f"https://{thost}", "bob", bob_pw, f"https://{mhost}", "--token", token], env=env.env)
            if result.returncode:
                env.dump_logs()
                for c in containers:
                    if c.endswith(("web", "sidekiq")):
                        print(f"--- {c} (tail) ---", flush=True)
                        subprocess.run(["sudo", "docker", "logs", "--tail", "40", c])
            return result.returncode
        finally:
            for c in containers:
                subprocess.run(["sudo", "docker", "rm", "-f", c], capture_output=True)
            env.close()


if __name__ == "__main__":
    if sys.argv[1:2] == ["--inside"]:
        sys.exit(inside(*sys.argv[2:4]))
    sys.exit(main())
