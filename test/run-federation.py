#!/usr/bin/env python3
"""Build trough and run federation checks on two disposable loopback instances."""
import contextlib
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

REPO = Path(__file__).resolve().parents[1]


def available_host():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return f"127.0.0.1:{sock.getsockname()[1]}"


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def ready(process, base):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"test instance exited with {process.returncode}")
        try:
            with urllib.request.urlopen(base + "/nodeinfo/2.0", timeout=1) as response:
                if response.status == 200:
                    return
        except (urllib.error.URLError, TimeoutError):
            pass
        time.sleep(0.1)
    raise RuntimeError(f"test instance did not become ready at {base}")


def main():
    with tempfile.TemporaryDirectory(prefix="trough-federation-") as temp:
        root = Path(temp)
        binary = root / "trough"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/trough"], cwd=REPO, check=True)
        env = os.environ.copy()
        # Never inherit production instance settings into the disposable test.
        for key in tuple(env):
            if key.startswith("TROUGH_"):
                del env[key]
        hosts = []
        with contextlib.ExitStack() as stack:
            logs = []
            try:
                for label, user in (("A", "alice"), ("B", "bob")):
                    host = available_host()
                    while host in hosts:
                        host = available_host()
                    hosts.append(host)
                    base = "http://" + host
                    password = secrets.token_urlsafe(24)
                    flags = ["-insecure", "-host", host, "-data", str(root / user)]
                    subprocess.run(
                        [str(binary), "user", "create", *flags, "-password-stdin", user],
                        input=password + "\n", text=True, stdout=subprocess.DEVNULL,
                        env=env, check=True,
                    )
                    log = stack.enter_context(open(root / f"{user}.log", "w+"))
                    logs.append(log)
                    process = subprocess.Popen(
                        [str(binary), "serve", *flags, "-listen", host],
                        stdout=log, stderr=subprocess.STDOUT, env=env,
                    )
                    stack.callback(stop, process)
                    ready(process, base)
                    env[f"TROUGH_TEST_{label}"] = base
                    env[f"TROUGH_TEST_{user.upper()}_PASSWORD"] = password
                subprocess.run(
                    [sys.executable, "-u", str(REPO / "test/federation.py")],
                    env=env, check=True, timeout=180,
                )
            except BaseException:
                for log in logs:
                    log.flush()
                    log.seek(0)
                    print(log.read(), file=sys.stderr)
                raise


if __name__ == "__main__":
    main()
