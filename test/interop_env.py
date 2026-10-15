"""Shared setup for the real-server interop runners (run-gotosocial.py, run-mastodon.py).

Everything listens on 127.0.0.1 only: this machine has no firewall.
"""
import contextlib
import os
from pathlib import Path
import secrets
import socket
import ssl
import subprocess
import sys
import time
import urllib.error
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


def require_signed_fetches(url, ca):
    """Check that a server in authorized-fetch mode refuses an unsigned fetch of url."""
    req = urllib.request.Request(url, headers={"Accept": "application/activity+json"})
    try:
        with urllib.request.urlopen(req, timeout=10, context=ssl.create_default_context(cafile=str(ca))) as r:
            raise RuntimeError(f"authorized fetch is on, but {url} was served unsigned ({r.status})")
    except urllib.error.HTTPError as e:
        if e.code not in (401, 403):
            raise
    print(f"{url} requires signed fetches", flush=True)


def docker(*args):
    r = subprocess.run(["sudo", "docker", *args], text=True, capture_output=True)
    if r.returncode:
        raise RuntimeError(f"docker {' '.join(args[-4:])} failed:\n{r.stdout[-2000:]}{r.stderr[-3000:]}")
    return r


def pull(*images):
    for image in images:
        if subprocess.run(["sudo", "docker", "image", "inspect", image], capture_output=True).returncode:
            docker("pull", image)


def in_private_netns(tag, image, script, *args):
    """Run `script --inside tag *args` as this user in a new network namespace
    that has only a loopback interface, held by a `--network none` container
    named <tag>-net that other containers can join. Returns its exit code.

    Many servers drop the port from a domain, so they can only federate on
    port 443; in here the test's Caddy can bind it without touching the host.
    """
    docker("run", "-d", "--rm", "--network", "none", "--name", f"{tag}-net", image, "sleep", "infinity")
    try:
        pid = docker("inspect", "-f", "{{.State.Pid}}", f"{tag}-net").stdout.strip()
        return subprocess.run([
            "sudo", "-E", "nsenter", f"--net=/proc/{pid}/ns/net",
            "setpriv", f"--reuid={os.getuid()}", f"--regid={os.getgid()}", "--init-groups",
            "env", f"PATH={os.environ['PATH']}",
            sys.executable, "-I", str(script), "--inside", tag, *args,
        ]).returncode
    finally:
        docker("rm", "-f", f"{tag}-net")


def build(binary):
    subprocess.run(["go", "build", "-o", str(binary), "."], cwd=REPO, check=True)


class Env:
    """A temp dir, processes to stop, and a local-CA Caddy for https."""

    def __init__(self, root, stack, env, tag=None):
        self.root, self.stack, self.env, self.tag = Path(root), stack, env, tag
        self.procs, self.containers = [], []

    def container(self, name, image, *cmd, detach=False, args=()):
        """docker run in the private namespace (see in_private_netns)."""
        run = ["run", "--network", f"container:{self.tag}-net", *args]
        if detach:  # kept after exiting for its logs; close() removes it
            run += ["-d", "--name", f"{self.tag}-{name}"]
            self.containers.append(f"{self.tag}-{name}")
        else:
            run.append("--rm")
        return docker(*run, image, *cmd)

    def postgres(self, image, port, user):
        """A throwaway PostgreSQL on 127.0.0.1:port with trust auth: nothing
        outside the private namespace can reach it."""
        self.container("pg", image, "-c", "listen_addresses=127.0.0.1", "-p", str(port), detach=True,
                       args=("-e", "POSTGRES_HOST_AUTH_METHOD=trust", "-e", f"POSTGRES_USER={user}"))
        for _ in range(60):
            if subprocess.run(["sudo", "docker", "exec", f"{self.tag}-pg", "pg_isready", "-h", "127.0.0.1", "-p", str(port)],
                              capture_output=True).returncode == 0:
                return
            time.sleep(1)
        raise RuntimeError("postgres did not start")

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
            print(f"--- {name} log (tail) ---\n" + "".join(log.readlines()[-lines:]), flush=True)
        for c in self.containers:
            print(f"--- {c} (tail) ---", flush=True)
            subprocess.run(["sudo", "docker", "logs", "--tail", str(lines), c])

    def close(self):
        for c in self.containers:
            subprocess.run(["sudo", "docker", "rm", "-f", c], capture_output=True)
        for _, p, _ in reversed(self.procs):
            stop(p)
