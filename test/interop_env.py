"""Shared setup for the real-server interop runners (run-gotosocial.py, run-mastodon.py).

Everything listens on 127.0.0.1 only: this machine has no firewall.
"""
import contextlib
from pathlib import Path
import secrets
import socket
import ssl
import subprocess
import time
import urllib.request

REPO = Path(__file__).resolve().parents[1]


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=20)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def wait_for(url, ca, process=None, timeout=120, want=b""):
    ctx = ssl.create_default_context(cafile=str(ca)) if ca else None
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process is not None and process.poll() is not None:
            raise RuntimeError(f"{process.args[0]} exited with {process.returncode}")
        with contextlib.suppress(Exception):
            with urllib.request.urlopen(url, timeout=3, context=ctx) as r:
                if r.status == 200 and want in r.read():
                    return
        time.sleep(0.5)
    raise RuntimeError(f"{url} did not become ready")


def build(binary):
    subprocess.run(["go", "build", "-o", str(binary), "."], cwd=REPO, check=True)


class Env:
    """A temp dir, processes to stop, and a local-CA Caddy for https."""

    def __init__(self, root, stack, env):
        self.root, self.stack, self.env = Path(root), stack, env
        self.procs = []

    def start(self, name, args, **kw):
        log = self.stack.enter_context(open(self.root / f"{name}.log", "w+"))
        p = subprocess.Popen(args, stdout=log, stderr=subprocess.STDOUT, env=self.env, **kw)
        self.procs.append((name, p, log))
        return p

    def caddy(self, sites):
        """sites: {"https host:port": backend port}. Returns the CA file."""
        conf = f"{{\n\tadmin off\n\tlocal_certs\n\tskip_install_trust\n\tauto_https disable_redirects\n\tstorage file_system {self.root}/caddy\n}}\n"
        for site, backend in sites.items():
            conf += f"https://{site} {{\n\ttls internal\n\treverse_proxy 127.0.0.1:{backend}\n}}\n"
        (self.root / "Caddyfile").write_text(conf)
        self.start("caddy", ["caddy", "run", "--config", str(self.root / "Caddyfile"), "--adapter", "caddyfile"])
        ca = self.root / "caddy/pki/authorities/local/root.crt"
        for _ in range(100):
            if ca.exists():
                break
            time.sleep(0.2)
        # trust both the local CA and the usual public roots
        bundle = self.root / "ca.pem"
        system = Path("/etc/ssl/certs/ca-certificates.crt")
        bundle.write_text(ca.read_text() + (system.read_text() if system.exists() else ""))
        self.env["SSL_CERT_FILE"] = str(bundle)
        return bundle

    def trough(self, host, port, binary=None):
        """Start trough (built here unless given) as https://host with user bob; returns bob's password."""
        if binary is None:
            binary = self.root / "trough"
            build(binary)
        flags = ["-insecure", "-scheme", "https", "-host", host, "-data", str(self.root / "trough-data")]
        pw = secrets.token_urlsafe(18)
        subprocess.run([str(binary), "user", "create", *flags, "-password-stdin", "bob"],
                       input=pw + "\n", text=True, env=self.env, check=True, capture_output=True)
        p = self.start("trough", [str(binary), "serve", *flags, "-listen", f"127.0.0.1:{port}"])
        return p, pw

    def dump_logs(self, lines=30):
        for name, _, log in self.procs:
            log.seek(0)
            print(f"--- {name} log (tail) ---\n" + "".join(log.readlines()[-lines:]))

    def close(self):
        for _, p, _ in reversed(self.procs):
            stop(p)
