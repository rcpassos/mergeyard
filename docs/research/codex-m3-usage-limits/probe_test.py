"""Offline checks at the probe's public check interface; no model requests."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
THREAD = "00000000-0000-4000-8000-000000000062"
MESSAGE = "You’ve hit your usage limit. Try again at 6:23 PM."


class EvidenceTests(unittest.TestCase):
    def check(self, events, rollout, exit_code=1, signal_sent=None):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            (directory / "events.jsonl").write_text(
                "".join(json.dumps(event) + "\n" for event in events))
            (directory / "run.json").write_text(json.dumps(
                {"exit_code": exit_code, "signal_sent": signal_sent}))
            (directory / "rollout-evidence.json").write_text(json.dumps(rollout))
            result = subprocess.run([sys.executable, str(HERE / "probe.py"),
                                     "check", str(directory)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            return json.loads(result.stdout)

    def test_first_request_limit_has_identity_but_no_structured_reset(self):
        outcome = self.check([
            {"type": "thread.started", "thread_id": THREAD},
            {"type": "turn.started"},
            {"type": "error", "message": MESSAGE},
            {"type": "turn.failed", "error": {"message": MESSAGE}},
        ], {"thread_id": THREAD, "found": True, "assistant_messages": 0,
            "records": [{"type": "event_msg", "payload": {
                "type": "task_complete", "error": {
                    "message": MESSAGE, "codex_error_info": "usage_limit_exceeded"}}}]})
        self.assertTrue(outcome["temporary_limit_observed"])
        self.assertEqual(outcome["thread_id"], THREAD)
        self.assertEqual(outcome["reset_source"], "message_only")
        self.assertEqual(outcome["reset_epochs"], [])
        self.assertIsNone(outcome["fallback_reset_at"])

    def test_successful_tool_text_cannot_establish_a_usage_limit(self):
        outcome = self.check([
            {"type": "thread.started", "thread_id": THREAD},
            {"type": "item.completed", "item": {"type": "command_execution", "aggregated_output": MESSAGE}},
            {"type": "turn.completed"},
        ], {"thread_id": THREAD, "found": True, "assistant_messages": 1, "records": []}, exit_code=0)
        self.assertFalse(outcome["temporary_limit_observed"])
        self.assertEqual(outcome["reset_source"], "unavailable")

    def test_unrelated_429_credit_and_interrupted_failures_are_unverified(self):
        for message, code, interruption in [
            ("Too many requests (429)", 1, None),
            ("Your workspace is out of credits. Add credits to continue.", 1, None),
            ("You hit your spend cap set in your workspace.", 1, None),
            (MESSAGE, -15, "watchdog_SIGTERM"),
        ]:
            with self.subTest(message=message):
                outcome = self.check([{"type": "turn.failed", "error": {"message": message}}],
                                     {"thread_id": None, "found": False, "records": []},
                                     exit_code=code, signal_sent=interruption)
                self.assertFalse(outcome["temporary_limit_observed"])

    def test_another_session_cannot_supply_the_reset_epoch(self):
        outcome = self.check([
            {"type": "thread.started", "thread_id": THREAD},
            {"type": "turn.failed", "error": {"message": MESSAGE}},
        ], {"thread_id": "different-session", "found": True, "records": [
            {"payload": {"type": "error", "codex_error_info": "usage_limit_exceeded"}},
            {"payload": {"type": "token_count", "rate_limits": {
                "primary": {"used_percent": 100, "resets_at": 1791048197}}}},
        ]})
        self.assertTrue(outcome["temporary_limit_observed"])
        self.assertFalse(outcome["rollout_found"])
        self.assertEqual(outcome["reset_epochs"], [])

    def test_missing_reset_requires_a_fallback_assumption_not_a_reported_time(self):
        outcome = self.check([{"type": "turn.failed", "error": {
            "message": "You've hit your usage limit. Try again later."}}],
            {"thread_id": None, "found": False, "records": []})
        self.assertTrue(outcome["temporary_limit_observed"])
        self.assertEqual(outcome["reset_source"], "unavailable")
        self.assertIsNone(outcome["fallback_reset_at"])

    def test_malformed_stream_does_not_pass_the_offline_check(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            (directory / "run.json").write_text('{"exit_code":1,"signal_sent":null}')
            (directory / "events.jsonl").write_text('{"type":"turn.failed"')
            result = subprocess.run([sys.executable, str(HERE / "probe.py"), "check", str(directory)],
                                    capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)

    def test_absent_latest_snapshot_does_not_reuse_an_earlier_reset(self):
        outcome = self.check([
            {"type": "thread.started", "thread_id": THREAD},
            {"type": "turn.failed", "error": {"message": MESSAGE}},
        ], {"thread_id": THREAD, "found": True, "assistant_messages": 0, "records": [
            {"payload": {"type": "token_count", "rate_limits": {
                "primary": {"used_percent": 100, "resets_at": 1791048197}}}},
            {"payload": {"type": "token_count", "rate_limits": None}},
            {"payload": {"type": "error", "codex_error_info": "usage_limit_exceeded"}},
        ]})
        self.assertEqual(outcome["reset_epochs"], [])

    def test_preserved_fixtures_match_their_explicit_provenance_and_contract(self):
        manifest = json.loads((HERE / "fixtures" / "manifest.json").read_text())
        for fixture in manifest:
            with self.subTest(fixture=fixture["directory"]):
                result = subprocess.run([sys.executable, str(HERE / "probe.py"), "check",
                                         str(HERE / "fixtures" / fixture["directory"])],
                                        check=True, capture_output=True, text=True)
                outcome = json.loads(result.stdout)
                for key in ("temporary_limit_observed", "reset_source", "reset_epochs"):
                    self.assertEqual(outcome[key], fixture[key])
                if outcome["temporary_limit_observed"]:
                    self.assertEqual(fixture["provenance"], "synthetic_unverified_variant")

    def test_limit_after_a_tool_item_is_not_evidence_of_first_request_rejection(self):
        outcome = self.check([
            {"type": "thread.started", "thread_id": THREAD},
            {"type": "turn.started"},
            {"type": "item.started", "item": {"type": "command_execution"}},
            {"type": "turn.failed", "error": {"message": MESSAGE}},
        ], {"thread_id": THREAD, "found": True, "assistant_messages": 0, "records": []})
        self.assertTrue(outcome["temporary_limit_observed"])
        self.assertFalse(outcome["first_request_limited"])


class CaptureTests(unittest.TestCase):
    def test_one_bounded_first_request_captures_only_its_rollout(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            source = parent / "auth-source"
            source.mkdir()
            (source / "auth.json").write_text('{"dummy":"offline-only"}')
            fake = parent / "fake-codex"
            fake.write_text(f"#!{sys.executable}\n" + r'''
import json, os, pathlib, sys
if "--version" in sys.argv:
    print("codex-cli offline-test")
    sys.exit(0)
home = pathlib.Path(os.environ["CODEX_HOME"])
assert (home / "auth.json").exists()
thread = "00000000-0000-4000-8000-000000000062"
message = "You've hit your usage limit. Try again at 6:23 PM."
records = [
    {"type":"session_meta","payload":{"id":thread,"private":"must not export"}},
    {"type":"response_item","payload":{"role":"user","content":"private prompt"}},
    {"type":"event_msg","payload":{"type":"token_count","info":None,"rate_limits":{
        "limit_id":"codex","primary":{"used_percent":100.0,"window_minutes":300,"resets_at":1791048197},
        "secondary":{"used_percent":20.0,"window_minutes":10080,"resets_at":1791648197}}}},
    {"type":"event_msg","payload":{"type":"error","message":message,"codex_error_info":"usage_limit_exceeded"}},
]
(home / "sessions").mkdir()
(home / "sessions" / ("rollout-test-"+thread+".jsonl")).write_text(
    "".join(json.dumps(record)+"\n" for record in records))
for event in [{"type":"thread.started","thread_id":thread},{"type":"turn.started"},
              {"type":"error","message":message},{"type":"turn.failed","error":{"message":message}}]:
    print(json.dumps(event),flush=True)
sys.exit(1)
''')
            fake.chmod(0o700)
            root = parent / "capture"
            env = dict(os.environ, CODEX_HOME=str(source), PYTHONDONTWRITEBYTECODE="1")
            command = [sys.executable, str(HERE / "probe.py"), "run", "--live",
                       "--root", str(root), "--codex", str(fake), "--timeout", "10"]
            result = subprocess.run(command, env=env, capture_output=True, text=True, timeout=15)
            self.assertEqual(result.returncode, 0, result.stderr)
            directory = root / "evidence" / "usage-limit"
            checked = subprocess.run([sys.executable, str(HERE / "probe.py"), "check",
                                      str(directory)], capture_output=True, text=True, check=True)
            outcome = json.loads(checked.stdout)
            self.assertTrue(outcome["temporary_limit_observed"])
            self.assertEqual(outcome["reset_epochs"], [1791048197])
            self.assertEqual(outcome["reset_source"], "rollout")
            self.assertTrue(outcome["first_request_limited"])
            self.assertFalse((root / "codex-home" / "auth.json").exists())
            self.assertTrue((source / "auth.json").exists())
            self.assertNotIn("private", (directory / "rollout-evidence.json").read_text())
            repeated = subprocess.run(command, env=env, capture_output=True, text=True)
            self.assertNotEqual(repeated.returncode, 0)


if __name__ == "__main__":
    unittest.main()
