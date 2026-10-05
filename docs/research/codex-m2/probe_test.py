"""Offline subprocess regression checks; never invoke the real Codex CLI."""

import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest


HERE = Path(__file__).resolve().parent


@unittest.skipUnless(os.name == "posix", "Probe process groups require POSIX")
class SupervisorTerminationTests(unittest.TestCase):
    def test_sigterm_stops_child_removes_auth_and_preserves_failure_evidence(self):
        with tempfile.TemporaryDirectory(prefix="codex-m2-offline-") as tmp:
            root = Path(tmp).resolve()
            source = root / "dummy-source"
            source.mkdir()
            (source / "auth.json").write_text('{"dummy": "offline-only"}\n')
            (root / "worktree").mkdir()
            (root / "fixture.json").write_text(json.dumps({"source": str(HERE), "calls": 0}))
            fake = root / "fake codex.py"
            fake.write_text(f"#!{sys.executable}\n" + '''
import json, os, pathlib, signal, sys
home = pathlib.Path(os.environ["CODEX_HOME"])
assert (home / "auth.json").exists()
signal.signal(signal.SIGTERM, signal.SIG_IGN)
(home.parent / "child.pid").write_text(str(os.getpid()))
result = {"schema_version": 1, "status": "success", "summary": "offline"}
pathlib.Path(sys.argv[sys.argv.index("-o") + 1]).write_text(json.dumps(result))
print(json.dumps({"type": "thread.started", "thread_id": "00000000-0000-4000-8000-000000000037"}), flush=True)
print(json.dumps({"type": "turn.completed"}), flush=True)
while True:
    signal.pause()
''')
            fake.chmod(0o700)
            env = dict(os.environ, CODEX_HOME=str(source), PYTHONDONTWRITEBYTECODE="1")
            supervisor = subprocess.Popen(
                [sys.executable, str(HERE / "probe.py"), "run", "--live",
                 "--root", str(root), "--case", "fresh", "--input", "unknown",
                 "--timeout", "10", "--codex", str(fake)],
                env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                text=True, start_new_session=True)
            child_pid = None
            directory = root / "evidence" / "fresh"
            try:
                deadline = time.monotonic() + 5
                while not (directory / "identity.json").exists():
                    if supervisor.poll() is not None or time.monotonic() > deadline:
                        self.fail("Fake CLI did not reach live identity capture")
                    time.sleep(0.02)
                child_pid = int((root / "child.pid").read_text())
                self.assertTrue((root / "codex-home" / "auth.json").exists())
                supervisor.terminate()  # Only the supervisor gets SIGTERM.
                stdout, stderr = supervisor.communicate(timeout=5)

                for name, observed in [
                    ("owned child stopped", not self.process_exists(child_pid)),
                    ("staged auth removed", not (root / "codex-home" / "auth.json").exists()),
                    ("run metadata saved", (directory / "run.json").exists()),
                    ("supervisor reports SIGTERM", supervisor.returncode == 143),
                ]:
                    with self.subTest(name=name):
                        self.assertTrue(observed, f"{name}; stdout={stdout!r}, stderr={stderr!r}")
                if (directory / "run.json").exists():
                    record = json.loads((directory / "run.json").read_text())
                    self.assertEqual(record["signal_sent"], "supervisor_SIGTERM")
                    self.assertEqual(record["exit_code"], -signal.SIGKILL)
                    self.assertEqual(record["thread_id"], "00000000-0000-4000-8000-000000000037")
                    checked = subprocess.run(
                        [sys.executable, str(HERE / "probe.py"), "check", str(directory)],
                        check=True, capture_output=True, text=True, env=env)
                    outcome = json.loads(checked.stdout)
                    self.assertTrue(outcome["schema_valid"])
                    self.assertFalse(outcome["native_success"])
                self.assertTrue((source / "auth.json").exists())
            finally:
                if supervisor.poll() is None:
                    supervisor.kill()
                supervisor.communicate(timeout=5)
                if child_pid is None and (root / "child.pid").exists():
                    child_pid = int((root / "child.pid").read_text())
                if child_pid is not None and self.process_exists(child_pid):
                    os.killpg(child_pid, signal.SIGKILL)

    @staticmethod
    def process_exists(pid):
        try:
            os.kill(pid, 0)
            return True
        except ProcessLookupError:
            return False


if __name__ == "__main__":
    unittest.main()
