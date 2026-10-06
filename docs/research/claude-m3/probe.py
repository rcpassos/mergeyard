#!/usr/bin/env python3
"""Explicit, single-invocation Claude usage-limit capture; never exhaust quota."""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time
import uuid


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--live", required=True, action="store_true",
                        help="Acknowledge one authenticated request that can use quota")
    parser.add_argument("--claude", default="claude")
    parser.add_argument("--model", default="sonnet")
    args = parser.parse_args()
    root = Path(tempfile.mkdtemp(prefix="mergeyard-claude-m3-")).resolve()
    root.chmod(0o700)
    workspace = root / "workspace"
    workspace.mkdir()
    version = subprocess.run([args.claude, "--version"], capture_output=True,
                             text=True, timeout=10, check=True).stdout.strip()
    session_id = str(uuid.uuid4())
    command = [args.claude, "-p", "--session-id", session_id,
               "--output-format", "stream-json", "--verbose",
               "--model", args.model, "--effort", "low", "--tools", "",
               "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}',
               "--setting-sources", "", "--settings", '{"disableAllHooks":true}',
               "--safe-mode", "--max-budget-usd", "0.10", "--max-turns", "1",
               "--", "Reply exactly OK. Do not use tools."]
    record = {"started_at": datetime.now(timezone.utc).isoformat(),
              "version": version, "argv": command, "cwd": str(workspace),
              "requested_session_id": session_id, "exit_code": None,
              "signal_sent": None, "elapsed_seconds": None,
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
        with (root / "events.jsonl").open("wb") as stdout, (root / "stderr.log").open("wb") as stderr:
            process = subprocess.Popen(command, cwd=workspace, stdin=subprocess.DEVNULL,
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
        if process is not None:
            # Also stop descendants if the leader exited before its watchdog.
            stop_group(signal.SIGKILL)
            record["exit_code"] = process.wait(timeout=5)
        record["elapsed_seconds"] = round(time.monotonic() - start, 2)
        write_json(root / "run.json", record)
        print(root, flush=True)


if __name__ == "__main__":
    main()
