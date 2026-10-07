#!/usr/bin/env python3
"""Opt-in terminal companion to the M2 supervisor; never called by CI."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import time

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("m2_probe", HERE.parent / "codex-m2" / "probe.py")
m2 = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m2)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--live", required=True, action="store_true")
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--session", required=True)
    parser.add_argument("--model", default="gpt-6-luna")
    parser.add_argument("--timeout", type=int, default=180, choices=range(10, 181))
    args = parser.parse_args()
    root = args.root.resolve()
    seed = json.loads((root / "evidence/sigint/run.json").read_text())
    if seed["thread_id"] != args.session or seed["signal_sent"] != "SIGINT" or seed["exit_code"] is None:
        raise SystemExit("Require the exact stopped SIGINT session and a completed supervisor record.")
    if not os.isatty(0) or not os.isatty(1):
        raise SystemExit("Run in a real terminal (or through script/tmux), not a pipe.")
    manifest = json.loads((root / "fixture.json").read_text())
    if manifest["source"] != str(m2.HERE) or manifest["calls"] >= 16:
        raise SystemExit("Wrong M2 fixture or invocation bound reached.")
    directory = root / "evidence/interactive"
    directory.mkdir()  # Never accept stale evidence.
    manifest["calls"] += 1
    m2.write_json(root / "fixture.json", manifest)
    home = root / "codex-home"
    auth = home / "auth.json"
    source = Path(os.environ.get("CODEX_HOME", str(Path.home() / ".codex")))
    env = dict(os.environ, CODEX_HOME=str(home))
    for key in ("OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"):
        env.pop(key, None)
    command = ["codex", "resume", "--no-daemon", "--no-alt-screen", "-C", str(root / "worktree"),
               "-m", args.model, "-s", "workspace-write", "-a", "on-request",
               "-c", "model_reasoning_effort=medium", "-c", "features.plugins=false",
               "-c", 'web_search="disabled"', "-c", "sandbox_workspace_write.network_access=false",
               "-c", "sandbox_workspace_write.exclude_slash_tmp=true",
               "-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true", args.session]
    m2.write_json(directory / "command.json", {"argv": command, "CODEX_HOME": str(home)})
    record = {"session_id": args.session, "seed_supervisor_finished": True,
              "exit_code": None, "signal_sent": None}
    process = None
    started = time.monotonic()

    termination_requested = False

    def cancelled(signum, frame):
        nonlocal termination_requested
        termination_requested = True
        record["signal_sent"] = "supervisor_SIGTERM"

    def abort_if_cancelled():
        if termination_requested:
            raise SystemExit(143)

    previous = signal.signal(signal.SIGTERM, cancelled)
    try:
        shutil.copyfile(source / "auth.json", auth)
        auth.chmod(0o600)
        abort_if_cancelled()
        process = subprocess.Popen(command, env=env, start_new_session=True)
        abort_if_cancelled()
        record["pid"] = process.pid
        try:
            deadline = started + args.timeout
            while process.poll() is None and time.monotonic() < deadline:
                abort_if_cancelled()
                try:
                    process.wait(timeout=min(0.2, max(0.01, deadline - time.monotonic())))
                except subprocess.TimeoutExpired:
                    pass
            abort_if_cancelled()
            if process.poll() is None:
                raise subprocess.TimeoutExpired(command, args.timeout)
            record["exit_code"] = process.returncode
        except subprocess.TimeoutExpired:
            record["signal_sent"] = "watchdog_SIGTERM"
            m2.signal_group(process, signal.SIGTERM)
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                m2.signal_group(process, signal.SIGKILL)
    finally:
        try:
            if process is not None:
                # Stop any descendants as well as the leader before handing back.
                m2.signal_group(process, signal.SIGKILL)
                record["exit_code"] = process.wait(timeout=5)
        finally:
            try:
                auth.unlink(missing_ok=True)
                signal.signal(signal.SIGTERM, signal.SIG_IGN)
                record["elapsed_seconds"] = round(time.monotonic() - started, 2)
                m2.write_json(directory / "run.json", record)
            finally:
                signal.signal(signal.SIGTERM, previous)
    print(json.dumps(record))
    if record["exit_code"] != 0 or record["signal_sent"]:
        raise SystemExit(1)



if __name__ == "__main__":
    main()
