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
import subprocess
import sys
import tempfile

sys.path.insert(0, str(Path(__file__).resolve().parent))
from interop_env import REPO, Env, build, free_port, in_private_netns, pull, require_signed_fetches, wait_for  # noqa: E402

MASTODON = os.environ.get("MASTODON_IMAGE", "ghcr.io/mastodon/mastodon:v4.7.3")
POSTGRES = "postgres:16-alpine"
REDIS = "redis:7-alpine"


def main():
    """Outside: build trough, then run inside() in a private network namespace."""
    tag = "trough-interop-" + secrets.token_hex(4)
    with tempfile.TemporaryDirectory(prefix="trough-mastodon-") as temp:
        binary = Path(temp) / "trough"
        build(binary)  # here, where the module proxy is reachable
        pull(MASTODON, POSTGRES, REDIS)
        return in_private_netns(tag, POSTGRES, __file__, str(binary))


def inside(tag, binary):
    with tempfile.TemporaryDirectory(prefix="trough-mastodon-") as temp, contextlib.ExitStack() as stack:
        env = Env(temp, stack, {k: v for k, v in os.environ.items() if not k.startswith("TROUGH_")}, tag)
        root = env.root
        pg, rd, web, tp = (free_port() for _ in range(4))
        mhost, thost = "localhost", "127.0.0.1"
        try:
            bundle = env.caddy({mhost: web, thost: tp})
            bundle.chmod(0o644)
            os.chmod(root, 0o755)
            menv = root / "mastodon.env"

            def run(name, *cmd, detach=False):
                return env.container(name, MASTODON, *cmd, detach=detach,
                                     args=("--env-file", str(menv), "-v", f"{bundle}:/ca.pem:ro"))

            env.postgres(POSTGRES, pg, "mastodon")
            env.container("redis", REDIS, "redis-server", "--bind", "127.0.0.1", "--port", str(rd), "--save", "", "--appendonly", "no", detach=True)
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
            run("setup", "bundle", "exec", "rails", "db:setup")
            # create the user directly: tootctl insists on a reachable mail domain,
            # and Mastodon resets `approved` on create
            ruby = ("u = User.new(email: 'mal@localhost', password: SecureRandom.hex(16), agreement: true,"
                    " account: Account.new(username: 'mal')); u.save!(validate: false);"
                    "u.update_columns(approved: true, confirmed_at: Time.now);"
                    "a = Doorkeeper::Application.create!(name: 'interop', redirect_uri: 'urn:ietf:wg:oauth:2.0:oob', scopes: 'read write follow push');"
                    "t = Doorkeeper::AccessToken.create!(application_id: a.id, resource_owner_id: u.id, scopes: 'read write follow push');"
                    "puts 'TOKEN=' + t.token")
            token = run("user", "bin/rails", "runner", ruby).stdout.split("TOKEN=", 1)[1].split()[0]
            run("web", "bundle", "exec", "puma", "-C", "config/puma.rb", detach=True)
            run("sidekiq", "bundle", "exec", "sidekiq", detach=True)

            tproc, bob_pw = env.trough(thost, tp, binary)
            wait_for(f"https://{thost}/nodeinfo/2.0", bundle, tproc)
            wait_for(f"https://{mhost}/api/v1/instance", bundle, timeout=240, want=b'"uri"')
            if os.environ.get("AUTHORIZED_FETCH") == "true":
                require_signed_fetches(f"https://{mhost}/users/mal", bundle)

            result = subprocess.run([sys.executable, "-I", str(REPO / "test/interop.py"),
                                     f"https://{thost}", "bob", bob_pw, f"https://{mhost}", "--token", token], env=env.env)
            if result.returncode:
                env.dump_logs()
            return result.returncode
        finally:
            env.close()


if __name__ == "__main__":
    if sys.argv[1:2] == ["--inside"]:
        sys.exit(inside(*sys.argv[2:4]))
    sys.exit(main())
