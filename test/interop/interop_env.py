"""Shared setup for the real-server interop runners (run-gotosocial.py, run-mastodon.py).

Everything listens on 127.0.0.1 only: this machine has no firewall.
"""
import contextlib
import os
from pathlib import Path
import secrets
import signal
import socket
import ssl
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

REPO = Path(__file__).resolve().parents[2]


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
        # Some resolvers (Node's with the ADDRCONFIG hint) return nothing without a
        # non-loopback interface: add a dummy one with documentation addresses and
        # no routes, so the namespace still cannot reach anything outside.
        for cmd in (["link", "add", "dummy0", "type", "dummy"], ["addr", "add", "192.0.2.1/32", "dev", "dummy0"],
                    ["addr", "add", "2001:db8::1/128", "dev", "dummy0", "nodad"], ["link", "set", "dummy0", "up"]):
            subprocess.run(["sudo", "nsenter", f"--net=/proc/{pid}/ns/net", "ip", *cmd], check=True)
        return subprocess.run([
            "sudo", "-E", "nsenter", f"--net=/proc/{pid}/ns/net",
            "setpriv", f"--reuid={os.getuid()}", f"--regid={os.getgid()}", "--init-groups",
            "env", f"PATH={os.environ['PATH']}",
            sys.executable, "-I", str(script), "--inside", tag, *args,
        ]).returncode
    finally:
        docker("rm", "-f", f"{tag}-net")


def build(binary):
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/trough"], cwd=REPO, check=True)


class Env:
    """A temp dir, processes to stop, and a local-CA Caddy for https."""

    def __init__(self, root, stack, env, tag=None):
        self.root, self.stack, self.env, self.tag = Path(root), stack, env, tag
        self.procs, self.containers = [], []
        signal.signal(signal.SIGTERM, self._terminate)

    @staticmethod
    def _terminate(*_):
        # still clean up, and don't let a second SIGTERM (sudo relays the one
        # sent to it too) interrupt that
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        sys.exit(143)

    def dns(self, names):
        """Answer DNS queries on 127.0.0.1:53 inside the private namespace:
        each of names gets A 127.0.0.1 and AAAA ::1, anything else NXDOMAIN.
        For servers whose resolver skips /etc/hosts. Returns a resolv.conf."""
        sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        sock.bind(("127.0.0.1", 53))
        self.stack.callback(sock.close)
        records = {b"\x00\x01": socket.inet_pton(socket.AF_INET, "127.0.0.1"),
                   b"\x00\x1c": socket.inet_pton(socket.AF_INET6, "::1")}

        def serve():
            while True:
                try:
                    q, addr = sock.recvfrom(512)
                except OSError:
                    return
                end, labels = 12, []
                while end < len(q) and q[end]:
                    labels.append(q[end + 1:end + 1 + q[end]].decode(errors="replace"))
                    end += 1 + q[end]
                qtype = q[end + 1:end + 3]
                known = ".".join(labels).lower() in names
                answer = b""
                if known and qtype in records:
                    rdata = records[qtype]
                    answer = b"\xc0\x0c" + qtype + b"\x00\x01\x00\x00\x00\x3c" + len(rdata).to_bytes(2, "big") + rdata
                header = q[:2] + bytes([0x81, 0x80 if known else 0x83]) + b"\x00\x01" + (b"\x00\x01" if answer else b"\x00\x00") + b"\x00\x00\x00\x00"
                sock.sendto(header + q[12:end + 5] + answer, addr)
        threading.Thread(target=serve, daemon=True).start()
        conf = self.root / "resolv.conf"
        conf.write_text("nameserver 127.0.0.1\n")
        conf.chmod(0o644)
        return conf

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
            conf += f"https://{site} {{\n\ttls internal\n\treverse_proxy 127.0.0.1:{backend}\n\tlog {{\n\t\toutput file {self.root}/access.log\n\t}}\n}}\n"
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
        try:
            if os.environ.get("INTEROP_PAUSE"):  # time to look around; end it early with SIGTERM
                print(f"pausing {os.environ['INTEROP_PAUSE']}s before cleanup; data in {self.root}", flush=True)
                time.sleep(int(os.environ["INTEROP_PAUSE"]))
        finally:
            for c in self.containers:
                subprocess.run(["sudo", "docker", "rm", "-f", c], capture_output=True)
            for _, p, _ in reversed(self.procs):
                stop(p)
