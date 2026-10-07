#!/usr/bin/env python3
"""Check the capabilities needed by Mergeyard's offline test suite."""

import argparse
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import uuid

LIVE_FLAGS = ("MERGEYARD_GITHUB_INTEGRATION", "MERGEYARD_GITHUB_PR_INTEGRATION",
              "MERGEYARD_SCHEDULER_INTEGRATION")
ROOT = Path(__file__).resolve().parents[1]


class Checks:
    """One probe deadline and ownership of the command currently running."""

    def __init__(self, seconds):
        self.deadline = time.monotonic() + seconds
        self.capability = "environment"
        self.remedy = "use an environment that permits the offline suite"
        self.owned = []
        self.foreground = set()
        self.starting = False
        self.cleaning = False
        self.interrupted = None
        self.pending = None

    def __enter__(self):
        self.handlers = {number: signal.getsignal(number)
                         for number in (signal.SIGTERM, signal.SIGINT, signal.SIGALRM)}
        for number in self.handlers:
            signal.signal(number, self.handle_signal)
        signal.setitimer(signal.ITIMER_REAL, max(0.001, self.deadline - time.monotonic()))
        return self

    def __exit__(self, *args):
        try:
            self.stop_owned()
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            for number, handler in self.handlers.items():
                signal.signal(number, handler)

    def stage(self, capability, remedy):
        self.capability, self.remedy = capability, remedy
        self.check()

    def handle_signal(self, number, frame):
        if self.cleaning:
            if number != signal.SIGALRM and self.pending is None:
                self.pending = "interrupted"
                self.interrupted = number
            if number == signal.SIGALRM and not self.starting:
                raise RuntimeError("cleanup deadline exceeded; inspect owned preflight resources")
            return
        if self.pending is None:
            self.pending = "deadline exceeded" if number == signal.SIGALRM else "interrupted"
            if number != signal.SIGALRM:
                self.interrupted = number
        # A signal during Popen is latched until its child handle is recorded.
        if not self.starting:
            self.check()

    def check(self):
        if self.pending or time.monotonic() >= self.deadline:
            raise RuntimeError(f"{self.capability}: {self.pending or 'deadline exceeded'}; {self.remedy}")

    def begin_cleanup(self):
        if not self.cleaning:
            self.cleaning = True
            self.deadline = time.monotonic() + 5
            signal.setitimer(signal.ITIMER_REAL, 5)

    def spawn(self, command, env=None, stdout=subprocess.PIPE, stderr=subprocess.PIPE, foreground=False):
        self.starting = True
        try:
            process = subprocess.Popen(command, env=env, stdin=subprocess.DEVNULL,
                                       stdout=stdout, stderr=stderr,
                                       text=True, start_new_session=True)
            self.owned.append(process)
            if foreground:
                self.foreground.add(process)
            return process
        except OSError as error:
            raise RuntimeError(f"{self.capability}: {error}; {self.remedy}") from error
        finally:
            self.starting = False

    def stop(self, process):
        self.begin_cleanup()
        # The handle retains ownership until wait. A reaped process must not
        # authorize signaling a possibly reused PID/group.
        if process.returncode is None:
            if process in self.foreground:
                # Server exit closes its panes; use the owned handle rather
                # than signaling an empty zombie group (EPERM on macOS).
                process.kill()
            else:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            process.wait(timeout=max(0.001, self.deadline - time.monotonic()))
        self.release(process)

    def release(self, process):
        for stream in (process.stdout, process.stderr):
            if stream is not None:
                stream.close()
        self.owned.remove(process)
        self.foreground.discard(process)

    def stop_owned(self):
        for process in list(self.owned):
            self.stop(process)

    def run(self, command, env=None, cleanup=False):
        if cleanup:
            self.begin_cleanup()
            # The shutdown RPC cannot spend the forced-cleanup reserve, even
            # when an earlier failed probe has already used part of the budget.
            command_deadline = min(time.monotonic() + 2, self.deadline - 3)
            if command_deadline <= time.monotonic():
                raise RuntimeError("tmux cleanup: time reserved for forced termination; allow owned-server shutdown")
        else:
            self.check()
            command_deadline = self.deadline
        previous = set(self.owned)
        failed = True
        try:
            process = self.spawn(command, env=env)
            if not cleanup:
                self.check()
            output, errors = process.communicate(timeout=max(0.001, command_deadline - time.monotonic()))
            if not cleanup:
                self.check()
            failed = False
            return subprocess.CompletedProcess(command, process.returncode, output, errors)
        except subprocess.TimeoutExpired as error:
            if cleanup:
                raise RuntimeError("tmux cleanup: shutdown command stalled; allow owned-server shutdown") from error
            raise RuntimeError(f"{self.capability}: deadline exceeded; {self.remedy}") from error
        finally:
            # Covers cancellation after Popen returns but before its handle is
            # assigned here, while preserving an already-owned tmux server.
            for process in list(self.owned):
                if process not in previous:
                    if failed:
                        self.stop(process)
                    else:
                        self.release(process)


def check_go(checks):
    checks.stage("Go", "install the required Go toolchain locally")
    required = re.search(r"^go ([0-9.]+)$", (ROOT / "go.mod").read_text(), re.MULTILINE).group(1)
    env = dict(os.environ, GOTOOLCHAIN="local", GOWORK="off", GOPROXY="off", GOSUMDB="off")
    result = checks.run(["go", "version"], env=env)
    found = re.search(r"\bgo(\d+)\.(\d+)(?:\.(\d+))?\b", result.stdout)
    minimum = tuple(int(part) for part in required.split("."))
    minimum += (0,) * (3 - len(minimum))
    if result.returncode or not found or tuple(int(part or 0) for part in found.groups()) < minimum:
        raise RuntimeError(f"Go: install Go {required} or later; observed {result.stdout.strip() or 'unavailable'}")
    return env


def check_writable(checks, name, value, allow_missing=False):
    checks.stage(name, "choose an accessible writable directory")
    path = Path(value)
    parent = path
    if allow_missing:
        while not parent.exists() and parent != parent.parent:
            parent = parent.parent
    try:
        with tempfile.TemporaryDirectory(prefix="my-pf-", dir=parent) as directory:
            probe = Path(directory) / "write-check"
            probe.write_text("temporary capability check")
            probe.read_text()
    except OSError as error:
        raise RuntimeError(f"{name}: {path} is not writable; choose an accessible writable directory ({error})") from error


def check_paths(checks, env):
    checks.stage("Go environment", "fix go env configuration or use a writable task cache")
    result = checks.run(["go", "env", "-json", "GOCACHE", "GOTMPDIR"], env=env)
    if result.returncode:
        raise RuntimeError(f"Go environment: fix go env configuration ({result.stderr.strip()})")
    try:
        values = json.loads(result.stdout)
    except ValueError as error:
        raise RuntimeError("Go environment: fix go env configuration; expected directory settings as JSON") from error
    if (not isinstance(values, dict) or not isinstance(values.get("GOCACHE"), str)
            or not isinstance(values.get("GOTMPDIR", ""), str)):
        raise RuntimeError("Go environment: fix GOCACHE/GOTMPDIR configuration; expected directory names")
    cache = values["GOCACHE"]
    if not cache or not Path(cache).is_absolute():
        raise RuntimeError("GOCACHE: choose an absolute writable build-cache directory")
    check_writable(checks, "GOCACHE", cache, allow_missing=True)
    temporary = os.environ.get("TMPDIR") or tempfile.gettempdir()
    check_writable(checks, "TMPDIR", temporary)
    go_temporary = values.get("GOTMPDIR") or temporary
    check_writable(checks, "GOTMPDIR", go_temporary)
    return temporary, go_temporary


def check_git(checks, directory):
    checks.stage("Git", "allow local repository operations in the temporary directory")
    if not shutil.which("git"):
        raise RuntimeError("Git: install Git and make it available on PATH")
    env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
    env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull, GIT_TERMINAL_PROMPT="0",
               GIT_AUTHOR_NAME="Preflight", GIT_AUTHOR_EMAIL="preflight@example.invalid",
               GIT_COMMITTER_NAME="Preflight", GIT_COMMITTER_EMAIL="preflight@example.invalid")
    repo = directory / "repository"
    repo.mkdir()
    command = ["git", "-c", "core.hooksPath=" + os.devnull, "-c", "init.templateDir=",
               "-c", "commit.gpgSign=false"]
    steps = [["init", "--quiet", str(repo)], ["-C", str(repo), "add", "--", "probe"],
             ["-C", str(repo), "commit", "--quiet", "-m", "Local capability check"],
             ["clone", "--quiet", "--no-hardlinks", str(repo), str(directory / "clone")],
             ["-C", str(repo), "worktree", "add", "--quiet", "--detach", str(directory / "linked")]]
    (repo / "probe").write_text("local Git capability check")
    for args in steps:
        result = checks.run(command + args, env=env)
        if result.returncode:
            raise RuntimeError(f"Git: {checks.remedy} ({result.stderr.strip()})")


def check_localhost(checks):
    checks.stage("localhost binding", "allow ephemeral localhost ports for the offline HTTP tests")
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
            listener.bind(("127.0.0.1", 0))
    except OSError as error:
        raise RuntimeError(f"localhost binding: {checks.remedy} ({error})") from error


def check_executable(checks, name, directory):
    checks.stage(name + " execution", "choose a writable directory that permits executable temporary files")
    script = directory / "exec-probe"
    script.write_text("#!/bin/sh\nexit 0\n")
    script.chmod(0o700)
    result = checks.run([str(script)])
    if result.returncode:
        raise RuntimeError(f"{name} execution: {checks.remedy} ({result.stderr.strip()})")


def check_tmux(checks, directory):
    checks.stage("tmux launch", "allow local sockets and process inspection")
    if not shutil.which("tmux"):
        raise RuntimeError("tmux: install tmux for the full offline suite; focused test commands remain available")
    label = "my-pf-" + uuid.uuid4().hex
    socket_root = Path(os.environ.get("TMUX_TMPDIR") or "/tmp") / ("tmux-" + str(os.getuid()))
    socket = socket_root / label
    if socket.exists():
        raise RuntimeError("tmux socket ownership: choose another isolated probe; preserve the existing socket")
    sentinel = directory / "identity-ok"
    script = directory / "tmux-probe.sh"
    script.write_text('''#!/bin/sh
set -eu
LC_ALL=C
export LC_ALL
started=$(/bin/ps -p $$ -o lstart=)
boot=$(/bin/ps -p 1 -o lstart=)
[ -n "$started" ] && [ -n "$boot" ]
[ "$(ps -p $$ -o lstart=)" = "$started" ]
[ "$(ps -p 1 -o lstart=)" = "$boot" ]
printf '%s\\n' "$$" >"$1"
exec /bin/sleep 30
''')
    command = ["tmux", "-L", label, "-f", os.devnull]
    client = command + ["-N"]  # Clients must never create an unowned daemon.
    completed = False
    with (directory / "tmux-server.log").open("w") as errors:
        try:
            server = checks.spawn(command + ["-D"], stdout=subprocess.DEVNULL, stderr=errors, foreground=True)
            while not socket.exists():
                checks.check()
                if server.poll() is not None:
                    diagnostic = (directory / "tmux-server.log").read_text().strip()
                    raise RuntimeError(f"tmux launch: allow an isolated foreground server ({diagnostic})")
                time.sleep(0.02)
            result = checks.run(client + ["new-session", "-d", "-s", "preflight", "--",
                                "/bin/sh", str(script), str(sentinel)])
            if result.returncode:
                raise RuntimeError(f"tmux launch: {checks.remedy} ({result.stderr.strip()})")
            checks.stage("tmux process identity", "allow /bin/ps and ps inspection of this process and PID 1")
            while not sentinel.exists():
                checks.check()
                time.sleep(0.02)
            completed = True
        finally:
            result = None
            try:
                result = checks.run(client + ["kill-server"], cleanup=True)
            finally:
                # Owning the foreground server gives a bounded fallback even
                # when its shutdown RPC fails. Confirm exit before unlinking.
                checks.stop_owned()
                socket.unlink(missing_ok=True)
                Path(str(socket) + ".lock").unlink(missing_ok=True)
            if completed and result.returncode:
                raise RuntimeError("tmux cleanup: allow owned-server shutdown; "
                                   "probe processes were forcibly stopped (" + result.stderr.strip() + ")")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--timeout", type=float, default=30, help="Shorten the 30-second probe deadline")
    args = parser.parse_args()
    if not 0 < args.timeout <= 30:
        parser.error("timeout must be greater than zero and at most 30 seconds")
    for flag in LIVE_FLAGS:
        if os.environ.get(flag) == "1":
            print(f"preflight: live integration: {flag}=1; unset it for make test. "
                  "Use the documented explicit live integration command instead.", file=sys.stderr)
            return 1
    checks = Checks(args.timeout)
    try:
        with checks:
            if sys.version_info < (3, 9):
                raise RuntimeError("Python: install Python 3.9 or later")
            env = check_go(checks)
            temporary, go_temporary = check_paths(checks, env)
            with tempfile.TemporaryDirectory(prefix="my-pf-", dir=temporary) as directory:
                check_git(checks, Path(directory))
                check_localhost(checks)
                check_executable(checks, "TMPDIR", Path(directory))
                if Path(go_temporary).resolve() != Path(temporary).resolve():
                    with tempfile.TemporaryDirectory(prefix="my-pf-", dir=go_temporary) as go_directory:
                        check_executable(checks, "GOTMPDIR", Path(go_directory))
                check_tmux(checks, Path(directory))
            if checks.interrupted:
                raise RuntimeError("interrupted; owned preflight resources cleaned")
    except (RuntimeError, OSError, ValueError, KeyError, subprocess.TimeoutExpired) as error:
        detail = f"{checks.capability}: {error}; {checks.remedy}" if isinstance(error, OSError) else str(error)
        print(f"preflight: {detail}", file=sys.stderr)
        return 128 + checks.interrupted if checks.interrupted else 1
    print("preflight: offline test environment ready")
    return 0


if __name__ == "__main__":
    sys.exit(main())
