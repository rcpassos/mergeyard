#!/usr/bin/env python3
"""Explicit, single-invocation Codex usage-limit capture; never exhaust quota."""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time


HERE = Path(__file__).resolve().parent
SCHEMA = HERE.parent / "codex-m2" / "schemas" / "implement.json"


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--live", required=True, action="store_true",
                        help="Acknowledge one authenticated request that can use quota")
    parser.add_argument("--codex", default="codex")
    parser.add_argument("--model", default="gpt-6-luna")
    args = parser.parse_args()
    root = Path(tempfile.mkdtemp(prefix="mergeyard-codex-m3-limit-")).resolve()
    root.chmod(0o700)
    workspace = root / "workspace"
    workspace.mkdir()
    home = root / "codex-home"
    home.mkdir(mode=0o700)
    auth = home / "auth.json"
    env = dict(os.environ, CODEX_HOME=str(home))
    for key in ("OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"):
        env.pop(key, None)
    version = subprocess.run([args.codex, "--version"], capture_output=True,
                             text=True, timeout=10, check=True).stdout.strip()
    # Mirrors the production exec contract (JSONL, native schema, last message)
    # with tools, rules, user configuration, and discovery disabled.
    command = [args.codex, "exec", "--json", "--ignore-user-config", "--ignore-rules",
               "--skip-git-repo-check", "-C", str(workspace), "-s", "read-only",
               "-m", args.model, "-c", "model_reasoning_effort=low",
               "-c", 'approval_policy="never"', "-c", 'web_search="disabled"',
               "-c", "features.shell_tool=false", "-c", "features.plugins=false",
               "-c", "features.apps=false", "-c", "features.hooks=false",
               "-c", "features.multi_agent=false", "-c", "project_doc_max_bytes=0",
               "--output-schema", str(SCHEMA), "-o", str(root / "last-message.json"),
               "--", "Return status success with summary OK. Do not use tools."]
    record = {"started_at": datetime.now(timezone.utc).isoformat(),
              "version": version, "argv": command, "cwd": str(workspace),
              "CODEX_HOME": str(home), "exit_code": None, "signal_sent": None,
              "thread_id": None, "elapsed_seconds": None,
              "watchdog_seconds": 90, "kill_grace_seconds": 5}
    write_json(root / "run.json", record)
    process = None
    start = time.monotonic()

    def stop_group(signum):
        try:
            os.killpg(process.pid, signum)
        except ProcessLookupError:
            pass

    try:
        source = Path(os.environ.get("CODEX_HOME", str(Path.home() / ".codex")))
        shutil.copyfile(source / "auth.json", auth)
        auth.chmod(0o600)
        with (root / "events.jsonl").open("wb") as stdout, (root / "stderr.log").open("wb") as stderr:
            process = subprocess.Popen(command, cwd=workspace, env=env, stdin=subprocess.DEVNULL,
                                       stdout=stdout, stderr=stderr, start_new_session=True)
            try:
                process.wait(timeout=90)
            except subprocess.TimeoutExpired:
                record["signal_sent"] = "watchdog_SIGTERM"
                stop_group(signal.SIGTERM)
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    record["signal_sent"] = "watchdog_SIGKILL"
                    stop_group(signal.SIGKILL)
                    process.wait(timeout=5)
    finally:
        try:
            if process is not None:
                # Also stop descendants if the leader exited before its watchdog.
                stop_group(signal.SIGKILL)
                record["exit_code"] = process.wait(timeout=5)
        finally:
            auth.unlink(missing_ok=True)
            record["elapsed_seconds"] = round(time.monotonic() - start, 2)
            for line in (root / "events.jsonl").read_text().splitlines():
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                if event.get("type") == "thread.started":
                    record["thread_id"] = event.get("thread_id")
            # Whether a limited first request leaves a resumable rollout behind.
            record["rollouts"] = sorted(str(p.relative_to(home))
                                        for p in (home / "sessions").rglob("*.jsonl"))
            write_json(root / "run.json", record)
            print(root, flush=True)


if __name__ == "__main__":
    main()
