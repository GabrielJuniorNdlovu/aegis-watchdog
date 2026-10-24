#!/usr/bin/env python3
"""Drive the real console through Linux PTYs; never contact a Docker daemon."""
import errno
import fcntl
import http.server
import json
import os
from pathlib import Path
import pty
import re
import select
import signal
import sqlite3
import struct
import subprocess
import sys
import termios
import threading
import time

BINARY = str(Path(sys.argv[1]).resolve())
ROOT = Path(sys.argv[2]).resolve()
ID = "a" * 64
REQUESTS = []
SLOW_STARTED = threading.Event()
SLOW_RELEASE = threading.Event()


class DockerFixture(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_HEAD(self):
        REQUESTS.append((self.command, self.path))
        self.send_response(200 if self.path == "/_ping" else 404)
        self.send_header("API-Version", "1.52")
        self.end_headers()

    def do_GET(self):
        REQUESTS.append((self.command, self.path))
        if self.path not in ("/v1.52/containers/demo/json", "/v1.52/containers/slow/json"):
            self.send_error(404)
            return
        if "/slow/" in self.path:
            SLOW_STARTED.set()
            SLOW_RELEASE.wait(15)
        data = json.dumps({"Id": ID, "Name": "/demo", "Config": {},
                           "State": {"Status": "running"}}).encode()
        try:
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def reject_mutation(self):
        REQUESTS.append((self.command, self.path))
        self.send_error(405)

    do_POST = do_DELETE = do_PUT = do_PATCH = reject_mutation


def controlling_terminal():
    os.setsid()
    fcntl.ioctl(0, termios.TIOCSCTTY, 0)


class Session:
    def __init__(self, env, db, width, height, command="console"):
        self.master, self.slave = pty.openpty()
        fcntl.ioctl(self.slave, termios.TIOCSWINSZ, struct.pack("HHHH", height, width, 0, 0))
        self.original = termios.tcgetattr(self.slave)
        args = [BINARY, command]
        if command == "console":
            args += ["--db", str(db)]
        self.process = subprocess.Popen(args, stdin=self.slave, stdout=self.slave,
                                        stderr=self.slave, env=env, preexec_fn=controlling_terminal)
        self.raw = bytearray()
        self.responded = 0

    def pump(self, seconds=0.05):
        if select.select([self.master], [], [], seconds)[0]:
            try:
                data = os.read(self.master, 65536)
            except OSError as exc:
                if exc.errno != errno.EIO:
                    raise
                return
            self.raw.extend(data)
            # A PTY has no terminal emulator to answer readline's cursor query.
            count = self.raw.count(b"\x1b[6n")
            while self.responded < count:
                os.write(self.master, b"\x1b[1;1R")
                self.responded += 1

    def expect(self, text, start=0):
        needle = text.encode()
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            where = self.raw.find(needle, start)
            if where >= 0:
                return where + len(needle)
            self.pump()
        raise AssertionError(f"missing {text!r}; captured tail: {bytes(self.raw[-1500:])!r}")

    def send(self, data):
        os.write(self.master, data)

    def command(self, data, result):
        start = len(self.raw)
        self.send(data)
        end = self.expect(result, start)
        self.expect("aegis> ", end)

    def finish(self, capture):
        deadline = time.monotonic() + 5
        while self.process.poll() is None and time.monotonic() < deadline:
            self.pump()
        assert self.process.poll() == 0, "console did not exit successfully"
        while select.select([self.master], [], [], 0)[0]:
            self.pump(0)
        assert termios.tcgetattr(self.slave) == self.original, "terminal state was not restored"
        capture.write_bytes(self.raw)

    def close(self):
        if self.process.poll() is None:
            self.process.kill()
            self.process.wait(timeout=5)
        os.close(self.master)
        os.close(self.slave)


def check_banner(session, env, width, height):
    welcome = Session(env, None, width, height, "welcome")
    try:
        welcome.finish(ROOT / f"welcome-{width}.ansi")
        banner = bytes(welcome.raw)
        assert session.raw.startswith(banner), "console changed the welcome artwork"
        assert session.raw.count(banner) == 1, "banner printed more than once"
        assert b"\xe2\x96\x88" in banner, "expected terminal block artwork"
    finally:
        welcome.close()


def main():
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), DockerFixture)
    server.daemon_threads = True
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    env = os.environ.copy()
    for name in ("DOCKER_API_VERSION", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_CONTEXT"):
        env.pop(name, None)
    env.update(DOCKER_HOST=f"tcp://127.0.0.1:{server.server_port}", TERM="xterm-256color",
               LC_ALL="C.UTF-8", NO_COLOR="1")
    sessions = []
    monitor = None
    try:
        absent = ROOT / "absent" / "journal.db"
        session = Session(env, absent, 120, 30)
        sessions.append(session)
        session.expect("aegis> ")
        check_banner(session, env, 120, 30)
        session.command(b"/status\r", "Controller status: unknown")
        session.send(b"/exit\r")
        session.finish(ROOT / "console-120.ansi")
        assert not absent.parent.exists(), "launch created controller data"
        assert not REQUESTS, "console launch or status contacted Docker"
        print("PASS: launch preserves banner and performs no Docker or journal writes")

        db = ROOT / "journal.db"
        subprocess.run([BINARY, "inspect", "--container", "demo", "--save", "--db", str(db)],
                       env=env, stdout=subprocess.DEVNULL, check=True, timeout=15)
        with (ROOT / "monitor.log").open("wb") as log:
            monitor = subprocess.Popen([BINARY, "monitor", "--container", "demo", "--interval", "200ms",
                                        "--db", str(db)], env=env, stdout=log, stderr=log)
        deadline = time.monotonic() + 5
        while True:
            with sqlite3.connect(f"file:{db}?mode=ro", uri=True) as connection:
                if connection.execute("SELECT count(*) FROM assessments").fetchone()[0]:
                    break
            assert time.monotonic() < deadline, "fixture monitor did not write assessments"
            time.sleep(0.02)

        session = Session(env, db, 80, 24)
        sessions.append(session)
        session.expect("aegis> ")
        check_banner(session, env, 80, 24)
        session.command(b"/hellp\x1b[D\x7f\r", "Console commands:")
        session.command(b"\x1b[A\r", "Console commands:")
        session.command(b"discard-this\x03/status\r", "Controller status: unknown")
        session.command(f"/history --container {ID} --limit 1\r".encode(), "historical; not current health")
        session.command(b"/decisions --container demo --limit 1\r", "Assessment: unknown")
        session.command(b"/invalid\r", "Unknown command")
        session.command(b"/actions --id 999\r", "does not exist")
        start = len(session.raw)
        session.send(b"/watch --container demo --interval 200ms\r")
        session.expect("Historical assessment read at:", start)
        start = len(session.raw)
        session.send(b"\x03")
        end = session.expect("Command canceled; returning to prompt.", start)
        session.expect("aegis> ", end)
        assert monitor.poll() is None, "watch stopped the independent monitor"
        start = len(session.raw)
        session.send(b"/inspect slow\r")
        assert SLOW_STARTED.wait(3), "slow inspection never reached fake Docker"
        session.send(b"\x03")
        end = session.expect("Command canceled; returning to prompt.", start)
        session.expect("aegis> ", end)
        SLOW_RELEASE.set()
        session.command(b"/help\r", "Console commands:")
        session.send(b"/exit\r")
        session.finish(ROOT / "console-80.ansi")
        assert not re.search(rb"\x1b\[[0-9;]*m", session.raw), "NO_COLOR output contains color SGR"
        assert b"\x1b[2J" not in session.raw, "console cleared the screen"
        assert monitor.poll() is None, "exiting the console stopped the monitor"
        print("PASS: editing, history, idle Ctrl+C, query errors, watch and inspection cancellation")

        session = Session(env, db, 60, 20)
        sessions.append(session)
        session.expect("aegis> ")
        check_banner(session, env, 60, 20)
        session.command(f"/history --container {ID} --limit 1\r".encode(), "historical; not current health")
        session.send(b"\x04")
        session.finish(ROOT / "console-60.ansi")
        session = Session(env, db, 60, 20)
        sessions.append(session)
        session.expect("aegis> ")
        session.process.send_signal(signal.SIGTERM)
        session.finish(ROOT / "console-sigterm.ansi")
        assert monitor.poll() is None
        rejected = subprocess.run([BINARY, "console"], input=b"/help\n", env=env,
                                  capture_output=True, timeout=5)
        assert rejected.returncode == 2 and not rejected.stdout and b"one-shot" in rejected.stderr
        assert all(method in ("HEAD", "GET") for method, _ in REQUESTS), REQUESTS
        print("PASS: 120/80/60-column layouts, Ctrl+D/SIGTERM terminal restoration, piped-input rejection")
        print("PASS: separate record-only monitor remains alive; fake Docker saw HEAD/GET requests only")
    finally:
        SLOW_RELEASE.set()
        for session in sessions:
            session.close()
        if monitor is not None and monitor.poll() is None:
            monitor.terminate()
            try:
                monitor.wait(timeout=5)
            except subprocess.TimeoutExpired:
                monitor.kill()
                monitor.wait(timeout=5)
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


if __name__ == "__main__":
    main()
