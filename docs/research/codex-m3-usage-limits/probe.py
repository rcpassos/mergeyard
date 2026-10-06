#!/usr/bin/env python3
"""Explicit, single-request Codex usage-limit research and offline evidence checks."""

import argparse
from datetime import datetime
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
from types import SimpleNamespace

HERE = Path(__file__).resolve().parent
M2 = HERE.parent / "codex-m2"
PROMPT = ('Do not use tools, read files, or do engineering work. '
          'Return schema_version 1, status success, summary "availability probe".')


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def read_events(path):
    return [json.loads(line) for line in path.read_text().splitlines()]


def capture_rollout(home, thread_id):
    evidence = {"thread_id": thread_id, "found": False,
                "assistant_messages": 0, "records": []}
    if not thread_id or not re.fullmatch(r"[0-9a-f-]{36}", thread_id):
        return evidence
    matches = list((home / "sessions").rglob("rollout-*-" + thread_id + ".jsonl"))
    if len(matches) != 1:
        return evidence
    records = read_events(matches[0])
    if not any(record.get("type") == "session_meta"
               and record.get("payload", {}).get("id") == thread_id for record in records):
        return evidence
    evidence["found"] = True
    for record in records:
        payload = record.get("payload", {})
        if record.get("type") == "response_item" and payload.get("role") == "assistant":
            evidence["assistant_messages"] += 1
        if record.get("type") != "event_msg":
            continue
        kind = payload.get("type")
        # Export only diagnostic fields, never prompts, tools, credentials or session metadata.
        selected = {"type": kind}
        if kind == "token_count":
            selected["rate_limits"] = payload.get("rate_limits")
        elif kind == "error":
            selected.update({key: payload[key] for key in ("message", "codex_error_info")
                             if key in payload})
        elif kind in ("task_complete", "turn_complete") and payload.get("error"):
            selected["error"] = {key: payload["error"][key]
                                 for key in ("message", "codex_error_info") if key in payload["error"]}
        else:
            continue
        evidence["records"].append({"type": "event_msg", "payload": selected})
    return evidence


def check(directory):
    run = json.loads((directory / "run.json").read_text())
    events = read_events(directory / "events.jsonl")
    rollout = json.loads((directory / "rollout-evidence.json").read_text())
    types = [event.get("type") for event in events]
    thread_ids = [event["thread_id"] for event in events if event.get("type") == "thread.started"]
    thread_id = thread_ids[0] if len(thread_ids) == 1 else None
    failures = [event.get("error", {}).get("message", "")
                for event in events if event.get("type") == "turn.failed"]
    limited = (run["exit_code"] == 1 and not run["signal_sent"]
               and "turn.completed" not in types
               and any("hit your usage limit" in message.lower() for message in failures))
    rollout_matches = bool(thread_id and rollout["found"] and rollout["thread_id"] == thread_id)
    epochs = []
    structured_limit = False
    if limited and rollout_matches:
        snapshots = []
        for record in rollout["records"]:
            payload = record["payload"]
            error = payload.get("error", payload)
            structured_limit |= error.get("codex_error_info") == "usage_limit_exceeded"
            if payload.get("type") == "token_count":
                snapshots.append(payload["rate_limits"])
        # Only the last snapshot is relevant; previous exhausted windows may be stale.
        if structured_limit and snapshots and snapshots[-1]:
            for key in ("primary", "secondary"):
                window = snapshots[-1].get(key) or {}
                reset = window.get("resets_at")
                if window.get("used_percent", 0) >= 100 and type(reset) is int and reset > 0:
                    epochs.append(reset)
    message_reset = limited and any("try again at " in message.lower() for message in failures)
    return {"temporary_limit_observed": limited, "exit_code": run["exit_code"],
            "events": types, "thread_id": thread_id, "rollout_found": rollout_matches,
            "structured_usage_limit": structured_limit, "reset_epochs": sorted(set(epochs)),
            "reset_source": "rollout" if epochs else "message_only" if message_reset else "unavailable",
            "fallback_reset_at": None,
            "first_request_limited": bool(limited and rollout_matches and types.count("turn.started") == 1
                                          and not any(kind and kind.startswith("item.") for kind in types)
                                          and rollout["assistant_messages"] == 0)}


def run(args):
    root = args.root.resolve()
    root.mkdir(mode=0o700)  # One invocation per root; refuse reuse or stale evidence.
    worktree = root / "worktree"
    worktree.mkdir()
    subprocess.run(["git", "init", "-q", str(worktree)], check=True)
    (root / "inputs").mkdir()
    (root / "inputs" / "availability.md").write_text(PROMPT + "\n")
    (root / "codex-home").mkdir(mode=0o700)
    version = subprocess.run([args.codex, "--version"], capture_output=True, text=True, timeout=10)
    write_json(root / "environment.json", {
        "version": version.stdout.strip(), "version_exit_code": version.returncode,
        "captured_at": datetime.now().astimezone().isoformat(),
        "timezone": os.environ.get("TZ"), "model": args.model, "prompt": PROMPT,
        "timeout_seconds": args.timeout, "maximum_invocations": 1})
    if version.returncode:
        raise SystemExit("Codex version lookup failed; no model request made.")
    # Reuse the already regression-tested process-group watchdog/auth cleanup,
    # not the M2 suite or its engineering inputs. Its fixed case path also rejects reuse.
    write_json(root / "fixture.json", {"source": str(M2), "calls": 0})
    spec = importlib.util.spec_from_file_location("m2_supervisor", M2 / "probe.py")
    supervisor = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(supervisor)
    settings = SimpleNamespace(root=root, case="usage-limit", no_auth=False,
                               schema="implement", worktree="worktree", sandbox="read-only",
                               model=args.model, effort="low", network=False, codex=args.codex,
                               bad_config=False, resume=None, skill=[], input="availability",
                               timeout=args.timeout, interrupt=None)
    try:
        supervisor.run(settings)
    finally:
        directory = root / "evidence" / "usage-limit"
        if (directory / "run.json").exists():
            record = json.loads((directory / "run.json").read_text())
            write_json(directory / "rollout-evidence.json",
                       capture_rollout(root / "codex-home", record.get("thread_id")))
    print(json.dumps(check(directory), indent=2))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    offline = commands.add_parser("check")
    offline.add_argument("directory", type=Path)
    live = commands.add_parser("run")
    live.add_argument("--live", required=True, action="store_true", help="Acknowledge quota use")
    live.add_argument("--root", type=Path, required=True, help="New private directory; must not exist")
    live.add_argument("--codex", default="codex")
    live.add_argument("--model", default="gpt-6-luna")
    live.add_argument("--timeout", type=int, default=30, choices=range(10, 61))
    args = parser.parse_args()
    if args.command == "check":
        print(json.dumps(check(args.directory), indent=2))
    else:
        run(args)


if __name__ == "__main__":
    main()
