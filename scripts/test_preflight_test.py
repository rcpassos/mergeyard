"""Public CLI/Make checks with isolated tools and no external services."""

import json
import os
import shutil
import signal
import time
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "test_preflight.py"
LIVE_FLAGS = ("MERGEYARD_GITHUB_INTEGRATION", "MERGEYARD_GITHUB_PR_INTEGRATION",
              "MERGEYARD_SCHEDULER_INTEGRATION")


class PreflightTests(unittest.TestCase):
    def setUp(self):
        self.fixture = tempfile.TemporaryDirectory(prefix="mergeyard-preflight-test-")
        self.addCleanup(self.fixture.cleanup)
        self.root = Path(self.fixture.name).resolve()
        self.sockets = tempfile.TemporaryDirectory(prefix="pf-sock-", dir="/tmp")
        self.addCleanup(self.sockets.cleanup)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.log = self.root / "commands.log"
        self.cache = self.root / "cache"
        self.cache.mkdir()
        self.tmp = self.root / "tmp"
        self.tmp.mkdir()
        self.env = dict(os.environ, TMUX_TMPDIR=self.sockets.name, TMPDIR=str(self.tmp), GOCACHE=str(self.cache), GOTMPDIR="", PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                        PREFLIGHT_TEST_LOG=str(self.log), PYTHONDONTWRITEBYTECODE="1")
        for flag in LIVE_FLAGS:
            self.env.pop(flag, None)
        self.tool("go", '''
import json, os, pathlib, sys
with pathlib.Path(os.environ["PREFLIGHT_TEST_LOG"]).open("a") as f:
    f.write("go " + " ".join(sys.argv[1:]) + "\\n")
if sys.argv[1] == "version":
    print("go version go1.24.6 linux/amd64")
elif sys.argv[1] == "env":
    assert os.environ["GOTOOLCHAIN"] == "local"
    assert os.environ["GOWORK"] == "off"
    assert os.environ["GOPROXY"] == "off"
    print(json.dumps({"GOCACHE":os.environ["GOCACHE"], "GOTMPDIR":os.environ.get("GOTMPDIR", "")}))
''')
        self.tool("git", "pass\n")
        self.tool("tmux", '''
import pathlib, sys
if "new-session" in sys.argv:
    pathlib.Path(sys.argv[-1]).write_text("offline fake identity")
''')
        (self.bin / "sitecustomize.py").write_text('''
import os, socket
native_bind = socket.socket.bind
def bind(self, address):
    if self.family in (socket.AF_INET, socket.AF_INET6):
        if os.environ.get("PREFLIGHT_TEST_BIND_DENIED") == "1":
            raise PermissionError("fixture denies localhost binding")
        return None
    return native_bind(self, address)
socket.socket.bind = bind
import pathlib, subprocess
native_popen = subprocess.Popen
native_wait = native_popen.wait
def wait(self, timeout=None):
    if (os.environ.get("PREFLIGHT_TEST_SLOW_TMUX_REAP") == "1"
            and "-D" in self.args and self.returncode is None):
        import time
        # Model a modest OS reaping delay at the subprocess boundary.
        time.sleep(0.02)
        if timeout is not None and timeout < 0.02:
            raise subprocess.TimeoutExpired(self.args, timeout)
    return native_wait(self, timeout=timeout)
native_popen.wait = wait
def popen(command, *args, **kwargs):
    denied = os.environ.get("PREFLIGHT_TEST_NOEXEC_PATH")
    executable = pathlib.Path(command[0])
    if denied and executable.is_absolute() and executable.is_relative_to(pathlib.Path(denied)):
        raise PermissionError("fixture denies executable temporary files")
    process = native_popen(command, *args, **kwargs)
    if os.environ.get("PREFLIGHT_TEST_CANCEL_START") == "1" and command[0] == "go":
        import signal
        pathlib.Path(os.environ["PREFLIGHT_TEST_CHILD"]).write_text(str(process.pid))
        os.kill(os.getpid(), signal.SIGTERM)
    return process
subprocess.Popen = popen
''')
        self.env["PYTHONPATH"] = str(self.bin)


    def tool(self, name, body):
        path = self.bin / name
        if name == "tmux":
            body = '''
import os, pathlib, sys, time
if "-D" in sys.argv:
    label = sys.argv[sys.argv.index("-L") + 1]
    socket_root = pathlib.Path(os.environ.get("TMUX_TMPDIR") or "/tmp") / ("tmux-" + str(os.getuid()))
    socket_root.mkdir(mode=0o700, exist_ok=True)
    if os.environ.get("PREFLIGHT_TEST_TMUX_PID"):
        pathlib.Path(os.environ["PREFLIGHT_TEST_TMUX_PID"]).write_text(str(os.getpid()))
    (socket_root / label).touch()
    time.sleep(60)
    sys.exit(0)
''' + body
        path.write_text(f"#!{sys.executable}\n" + body)
        path.chmod(0o700)

    def run_script(self, *args):
        return subprocess.run([sys.executable, str(SCRIPT), *args], env=self.env,
                              cwd=ROOT, capture_output=True, text=True, timeout=10)

    def test_make_rejects_live_flags_before_starting_any_suite(self):
        for flag in LIVE_FLAGS:
            with self.subTest(flag=flag):
                self.env[flag] = "1"
                result = subprocess.run(["make", "test"], cwd=ROOT, env=self.env,
                                        capture_output=True, text=True, timeout=10)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(flag, result.stdout + result.stderr)
                self.assertFalse(self.log.exists(), "Go suite started before preflight rejected live mode")
                self.env.pop(flag)

    def test_unsupported_go_names_the_required_version(self):
        self.tool("go", 'print("go version go1.23.12 linux/amd64")\n')
        result = self.run_script()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Go", result.stderr)
        self.assertIn("1.24", result.stderr)
        self.assertIn("install", result.stderr.lower())

    def test_configured_cache_and_temp_paths_fail_with_a_remedy(self):
        invalid = self.root / "file-instead-of-directory"
        invalid.write_text("preserve this file")
        for variable in ("GOCACHE", "TMPDIR", "GOTMPDIR"):
            with self.subTest(variable=variable):
                previous = self.env[variable]
                self.env[variable] = str(invalid)
                result = self.run_script()
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(variable, result.stderr)
                self.assertIn("writable", result.stderr)
                self.assertEqual(invalid.read_text(), "preserve this file")
                self.env[variable] = previous

    def test_local_git_failure_is_reported_before_the_suite(self):
        self.tool("git", 'import sys\nprint("cannot create local repository", file=sys.stderr)\nsys.exit(1)\n')
        result = self.run_script()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Git", result.stderr)
        self.assertIn("temporary", result.stderr)

    def test_missing_tmux_fails_instead_of_skipping_coverage(self):
        (self.bin / "tmux").unlink()
        self.env["PATH"] = str(self.bin)
        result = self.run_script()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("tmux", result.stderr)
        self.assertIn("install", result.stderr.lower())

    def test_installed_but_unusable_tmux_names_the_missing_capability(self):
        self.tool("tmux", 'import sys\nprint("socket creation denied", file=sys.stderr)\nsys.exit(1)\n')
        result = self.run_script()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("tmux", result.stderr)
        self.assertIn("launch", result.stderr)
        self.assertEqual(list(self.tmp.iterdir()), [], "Failed preflight leaked its temporary resources")

    def test_deadline_stops_a_slow_probe_and_cleans_resources(self):
        self.tool("go", 'import time\ntime.sleep(1.5)\nprint("go version go1.24.6 linux/amd64")\n')
        started = time.monotonic()
        result = self.run_script("--timeout", "0.2")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("deadline", result.stderr)
        self.assertLess(time.monotonic() - started, 2)
        self.assertEqual(list(self.tmp.iterdir()), [])

    def test_denied_localhost_bind_fails_with_a_remedy(self):
        self.env["PREFLIGHT_TEST_BIND_DENIED"] = "1"
        result = self.run_script()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("localhost", result.stderr)
        self.assertIn("allow", result.stderr)
        self.assertEqual(list(self.tmp.iterdir()), [])

    def test_temporary_directories_must_allow_executable_files(self):
        go_tmp = self.root / "go tmp with spaces"
        go_tmp.mkdir()
        self.env["GOTMPDIR"] = str(go_tmp)
        for variable, directory in (("TMPDIR", self.tmp), ("GOTMPDIR", go_tmp)):
            with self.subTest(variable=variable):
                self.env["PREFLIGHT_TEST_NOEXEC_PATH"] = str(directory)
                result = self.run_script()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(variable, result.stderr)
                self.assertIn("executable", result.stderr)
                self.assertEqual(list(directory.iterdir()), [])

    def test_ready_environment_preserves_configured_paths_and_readonly_modules(self):
        modules = self.root / "warm modules"
        modules.mkdir()
        marker = modules / "keep"
        marker.write_text("cached dependency")
        modules.chmod(0o500)
        self.addCleanup(modules.chmod, 0o700)
        self.env["GOMODCACHE"] = str(modules)
        self.env["MERGEYARD_GITHUB_INTEGRATION"] = "0"
        cached = self.cache / "keep"
        cached.write_text("cached build")
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("ready", result.stdout)
        self.assertEqual(list(self.tmp.iterdir()), [])
        self.assertEqual(list(self.cache.iterdir()), [cached])
        self.assertEqual(cached.read_text(), "cached build")
        self.assertEqual(marker.read_text(), "cached dependency")
        self.assertNotIn("build", self.log.read_text())
        self.assertNotIn("test", self.log.read_text())

    @unittest.skipUnless(shutil.which("tmux") and shutil.which("go"), "Real smoke needs Go and tmux")
    def test_real_isolated_tmux_smoke_preserves_the_configured_environment(self):
        self.env["PATH"] = os.environ["PATH"]
        self.env.pop("PYTHONPATH", None)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("ready", result.stdout)
        self.assertEqual(list(self.tmp.iterdir()), [])
        self.assertEqual(list(self.cache.iterdir()), [])

    def test_missing_git_explains_how_to_restore_the_environment(self):
        (self.bin / "git").unlink()
        self.env["PATH"] = str(self.bin)
        result = self.run_script()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Git", result.stderr)
        self.assertIn("install", result.stderr.lower())

    def test_unreadable_go_environment_has_a_capability_and_remedy(self):
        for output in ("not JSON", "[]", '{"GOCACHE":false}', '{}'):
            with self.subTest(output=output):
                self.tool("go", 'import sys\nif sys.argv[1] == "version": print("go version go1.24.6 linux/amd64")\nelse: print(' + repr(output) + ')\n')
                result = self.run_script()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Go environment", result.stderr)
                self.assertIn("fix", result.stderr)
                self.assertNotIn("Traceback", result.stderr)

    def test_sigterm_during_cleanup_is_retained_in_the_result(self):
        self.tool("tmux", '''
import os, pathlib, signal, sys
if "new-session" in sys.argv:
    pathlib.Path(sys.argv[-1]).write_text("offline identity")
else:
    os.kill(os.getppid(), signal.SIGTERM)
''')
        result = self.run_script()
        self.assertEqual(result.returncode, 143, result.stdout + result.stderr)
        self.assertIn("interrupted", result.stderr)
        self.assertNotIn("ready", result.stdout)
        self.assertEqual(list(self.tmp.iterdir()), [])

    def assert_stopped(self, pid):
        state = subprocess.run(["/bin/ps", "-p", str(pid), "-o", "stat="],
                               capture_output=True, text=True)
        self.assertTrue(state.returncode != 0 or state.stdout.strip().startswith("Z"),
                        "Owned probe process is still running: " + state.stdout)

    def test_sigterm_during_process_creation_stops_the_owned_child(self):
        pid_file = self.root / "child.pid"
        self.env["PREFLIGHT_TEST_CHILD"] = str(pid_file)
        self.env["PREFLIGHT_TEST_CANCEL_START"] = "1"
        self.tool("go", 'import time\ntime.sleep(60)\n')
        result = self.run_script()
        self.assertEqual(result.returncode, 143, result.stdout + result.stderr)
        self.assertIn("interrupted", result.stderr)
        self.assert_stopped(int(pid_file.read_text()))
        self.assertEqual(list(self.tmp.iterdir()), [])

    def test_deadline_stops_a_descendant_after_its_parent_exits(self):
        pid_file = self.root / "child.pid"
        self.env["PREFLIGHT_TEST_CHILD"] = str(pid_file)
        self.tool("go", '''
import os, pathlib, subprocess, sys
child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"])
pathlib.Path(os.environ["PREFLIGHT_TEST_CHILD"]).write_text(str(child.pid))
sys.exit(0)
''')
        result = self.run_script("--timeout", "1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("deadline", result.stderr)
        self.assert_stopped(int(pid_file.read_text()))
        self.assertEqual(list(self.tmp.iterdir()), [])

    def test_failed_tmux_shutdown_never_reports_ready_or_leaves_its_probe(self):
        pid_file = self.root / "tmux.pid"
        self.env["PREFLIGHT_TEST_TMUX_PID"] = str(pid_file)
        self.tool("tmux", '''
import os, pathlib, sys
pid_file = pathlib.Path(os.environ["PREFLIGHT_TEST_TMUX_PID"])
if "new-session" in sys.argv:
    assert pid_file.exists()
    pathlib.Path(sys.argv[-1]).write_text("offline identity")
elif "kill-server" in sys.argv:
    print("shutdown denied", file=sys.stderr)
    sys.exit(1)
''')
        try:
            result = self.run_script()
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("cleanup", result.stderr)
            self.assertNotIn("ready", result.stdout)
            self.assert_stopped(int(pid_file.read_text()))
            self.assertEqual(list(self.tmp.iterdir()), [])
        finally:
            if pid_file.exists():
                try:
                    os.kill(int(pid_file.read_text()), signal.SIGKILL)
                except ProcessLookupError:
                    pass

    def test_stalled_tmux_shutdown_reserves_forced_cleanup_and_unlinks_socket(self):
        pid_file = self.root / "tmux.pid"
        shutdown_started = self.root / "shutdown-started"
        self.env["PREFLIGHT_TEST_TMUX_PID"] = str(pid_file)
        self.env["PREFLIGHT_TEST_SHUTDOWN_STARTED"] = str(shutdown_started)
        self.env["PREFLIGHT_TEST_SLOW_TMUX_REAP"] = "1"
        self.tool("tmux", '''
import os, pathlib, sys, time
if "new-session" in sys.argv:
    pathlib.Path(sys.argv[-1]).write_text("offline identity")
elif "kill-server" in sys.argv:
    pathlib.Path(os.environ["PREFLIGHT_TEST_SHUTDOWN_STARTED"]).write_text(str(time.monotonic()))
    time.sleep(60)
''')
        result = self.run_script()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("shutdown command stalled", result.stderr)
        self.assertNotIn("ready", result.stdout)
        self.assert_stopped(int(pid_file.read_text()))
        socket_dir = Path(self.sockets.name) / ("tmux-" + str(os.getuid()))
        self.assertEqual(list(socket_dir.iterdir()), [], "Stalled shutdown left an owned socket behind")
        self.assertEqual(list(self.tmp.iterdir()), [])
        self.assertLess(time.monotonic() - float(shutdown_started.read_text()), 5)


if __name__ == "__main__":
    unittest.main()
