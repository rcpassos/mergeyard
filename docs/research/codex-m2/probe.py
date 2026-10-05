#!/usr/bin/env python3
"""Manual, bounded Codex evidence collection. Never imported by automated tests."""

import argparse
import json
import os
from pathlib import Path
import selectors
import shutil
import signal
import subprocess
import tempfile
import time


HERE = Path(__file__).resolve().parent


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def git(root, *args):
    subprocess.run(["git", "-C", str(root), *args], check=True, capture_output=True)


def prepare():
    root = Path(tempfile.mkdtemp(prefix="mergeyard-codex-m2-")).resolve()
    root.chmod(0o700)
    base = root / "base"
    base.mkdir()
    git(base, "init", "-b", "main")
    git(base, "config", "user.name", "Harness Probe")
    git(base, "config", "user.email", "probe@example.invalid")
    (base / "go.mod").write_text("module example.invalid/probe\n\ngo 1.24\n")
    (base / "value.go").write_text("package probe\n\nfunc Value() int { return 1 }\n")
    (base / "value_test.go").write_text(
        'package probe\n\nimport "testing"\n\n'
        'func TestValue(t *testing.T) { if Value() != 2 { t.Fatal("want 2") } }\n')
    shutil.copytree(HERE / "skills", base / ".agents" / "skills")
    git(base, "add", ".")
    git(base, "commit", "-m", "Seed isolated probe")
    git(base, "worktree", "add", "-b", "probe", str(root / "worktree"))
    git(base, "worktree", "add", "-b", "other", str(root / "other"))
    shutil.copytree(HERE / "inputs", root / "inputs")
    (root / "codex-home").mkdir(mode=0o700)
    # Auth is staged only during a run, then removed. No user config is copied.
    write_json(root / "fixture.json", {"source": str(HERE), "calls": 0})
    print(root)


def validate(value, schema):
    """The subset used by these fixtures; deliberately rejects extra/missing fields."""
    kind = schema.get("type")
    kinds = kind if isinstance(kind, list) else [kind]
    valid = {"object": isinstance(value, dict), "array": isinstance(value, list),
             "string": isinstance(value, str), "null": value is None,
             "integer": type(value) is int}
    if not any(valid.get(k, False) for k in kinds):
        return False
    if "const" in schema and value != schema["const"]:
        return False
    if "enum" in schema and value not in schema["enum"]:
        return False
    if isinstance(value, dict):
        props = schema["properties"]
        if set(value) != set(props):
            return False
        return all(validate(value[k], props[k]) for k in value)
    if isinstance(value, list):
        return all(validate(v, schema["items"]) for v in value)
    return True


def check(directory, schema_path):
    record = json.loads((directory / "run.json").read_text())
    events = [json.loads(line) for line in (directory / "events.jsonl").read_text().splitlines()]
    result = directory / "last-message.json"
    try:
        value = json.loads(result.read_text())
        schema_valid = validate(value, json.loads(schema_path.read_text()))
    except (OSError, ValueError):
        schema_valid = False
    types = [event.get("type") for event in events]
    # Neither a result file alone nor exit 0 alone establishes native completion.
    complete = (record["exit_code"] == 0 and not record["signal_sent"]
                and "turn.completed" in types and "turn.failed" not in types
                and "error" not in types and schema_valid)
    return {"native_success": complete, "schema_valid": schema_valid, "events": types}


def signal_group(process, signum):
    # The group may outlive its leader while a descendant holds stdout open.
    try:
        os.killpg(process.pid, signum)
    except ProcessLookupError:
        pass


def run(args):
    root = args.root.resolve()
    manifest = json.loads((root / "fixture.json").read_text())
    if manifest["source"] != str(HERE) or manifest["calls"] >= 16:
        raise SystemExit("Wrong fixture or 16-invocation bound reached; prepare a new fixture.")
    directory = root / "evidence" / args.case
    directory.mkdir(parents=True)  # Refuse reuse: stale result files cannot pass.
    manifest["calls"] += 1
    write_json(root / "fixture.json", manifest)
    schema = HERE / "schemas" / (args.schema + ".json")
    home = root / ("unauthenticated-home" if args.no_auth else "codex-home")
    home.mkdir(exist_ok=True, mode=0o700)
    auth = home / "auth.json"
    env = dict(os.environ, CODEX_HOME=str(home))
    for key in ("OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"):
        env.pop(key, None)
    command = [args.codex, "exec", "--ignore-user-config", "--ignore-rules",
               "--json", "-C", str(root / args.worktree), "-s", args.sandbox,
               "-m", args.model, "-c", "model_reasoning_effort=" + args.effort,
               "-c", "sandbox_workspace_write.network_access=" + str(args.network).lower(),
               "-c", "features.plugins=false", "-c", "web_search=\"disabled\"",
               "--output-schema", str(schema), "-o", str(directory / "last-message.json")]
    if args.bad_config:
        command += ["-c", "model_reasoning_effort=invalid-probe-value"]
    if args.resume:
        command += ["resume", args.resume]
    prompt = " ".join("$" + skill for skill in args.skill)
    prompt += f" Read {root / 'inputs' / (args.input + '.md')} and follow it. Do not commit."
    command.append(prompt)
    write_json(directory / "command.json", {"argv": command, "CODEX_HOME": str(home)})
    start = time.monotonic()
    record = {"exit_code": None, "signal_sent": None, "thread_id": None,
              "identity_before_exit": False, "elapsed_seconds": None}
    process = None
    selector = None
    group_kill_sent = False
    termination_requested = False

    def supervisor_sigterm(signum, frame):
        nonlocal termination_requested
        # Latch cancellation so Popen can return its child handle before cleanup.
        termination_requested = True
        record["signal_sent"] = "supervisor_SIGTERM"

    def abort_if_terminated():
        if termination_requested:
            raise SystemExit(128 + signal.SIGTERM)

    previous_sigterm = signal.signal(signal.SIGTERM, supervisor_sigterm)
    try:
        if not args.no_auth:
            source = Path(os.environ.get("CODEX_HOME", str(Path.home() / ".codex")))
            shutil.copyfile(source / "auth.json", auth)
            auth.chmod(0o600)
            if (source / "models_cache.json").exists():
                shutil.copyfile(source / "models_cache.json", home / "models_cache.json")
        abort_if_terminated()
        with (directory / "events.jsonl").open("w") as stdout, (directory / "stderr.log").open("w") as stderr:
            process = subprocess.Popen(command, env=env, stdin=subprocess.DEVNULL,
                                       stdout=subprocess.PIPE, stderr=stderr, start_new_session=True)
            selector = selectors.DefaultSelector()
            selector.register(process.stdout, selectors.EVENT_READ)
            pending = b""
            while selector.get_map():
                abort_if_terminated()
                elapsed = time.monotonic() - start
                if elapsed > args.timeout and not record["signal_sent"]:
                    record["signal_sent"] = "watchdog_SIGTERM"
                    signal_group(process, signal.SIGTERM)
                if elapsed > args.timeout + 5:
                    signal_group(process, signal.SIGKILL)
                    group_kill_sent = True
                    # Even a descendant outside the group must not hold capture open.
                    break
                for key, _ in selector.select(timeout=0.2):
                    chunk = os.read(key.fileobj.fileno(), 65536)
                    if not chunk:
                        selector.unregister(key.fileobj)
                        continue
                    pending += chunk
                    while b"\n" in pending:
                        line, pending = pending.split(b"\n", 1)
                        stdout.write(line.decode() + "\n")
                        stdout.flush()
                        event = json.loads(line)
                        if event.get("type") == "thread.started":
                            record["thread_id"] = event["thread_id"]
                            record["identity_before_exit"] = process.poll() is None
                            write_json(directory / "identity.json", record)
                            print("Captured live identity:", event["thread_id"], flush=True)
                        item = event.get("item", {})
                        if (args.interrupt and event.get("type") == "item.started"
                                and "sleep 30" in item.get("command", "") and not record["signal_sent"]):
                            record["signal_sent"] = args.interrupt
                            signal_group(process, getattr(signal, args.interrupt))
                if process.poll() is not None and not selector.get_map():
                    break
            if pending:
                stdout.write(pending.decode())
            abort_if_terminated()
            record["exit_code"] = process.wait(timeout=5)
    finally:
        try:
            capture_open = selector is not None and bool(selector.get_map())
            if selector is not None:
                selector.close()
            if process is not None:
                if not group_kill_sent and (capture_open or process.poll() is None):
                    signal_group(process, signal.SIGKILL)
                record["exit_code"] = process.wait(timeout=5)
                process.stdout.close()
        finally:
            # Preserve evidence and remove staged auth even if shutdown is denied.
            try:
                auth.unlink(missing_ok=True)
                # Work and credential cleanup are done; freeze cancellation state
                # while publishing its snapshot so late SIGTERM cannot be lost.
                signal.signal(signal.SIGTERM, signal.SIG_IGN)
                record["elapsed_seconds"] = round(time.monotonic() - start, 2)
                write_json(directory / "run.json", record)
            finally:
                signal.signal(signal.SIGTERM, previous_sigterm)
    abort_if_terminated()
    # Keep the seed accessible only in history on subsequent turns.
    if args.input in ("implement", "interrupt"):
        (root / "inputs" / (args.input + ".md")).unlink()
    print(json.dumps(record))
    print(json.dumps(check(directory, schema)))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("prepare")
    offline = commands.add_parser("check")
    offline.add_argument("directory", type=Path)
    offline.add_argument("--schema", default="implement", choices=["implement", "review", "fix"])
    live = commands.add_parser("run")
    live.add_argument("--live", required=True, action="store_true", help="Acknowledge paid model calls")
    live.add_argument("--root", type=Path, required=True)
    live.add_argument("--case", required=True, choices=["implement", "review", "fix", "fix-retry", "settings", "review-resume", "unknown", "fresh", "bad-schema", "bad-config", "no-auth", "sigint", "sigint-resume", "sigterm", "sigterm-resume", "cache-network"])
    live.add_argument("--input", required=True, choices=[p.stem for p in (HERE / "inputs").glob("*.md")])
    live.add_argument("--schema", default="implement", choices=["implement", "review", "fix", "invalid"])
    live.add_argument("--resume")
    live.add_argument("--skill", action="append", default=[], choices=["m2-amber", "m2-blue"])
    live.add_argument("--model", default="gpt-6-luna")
    live.add_argument("--effort", default="low")
    live.add_argument("--sandbox", default="workspace-write", choices=["workspace-write", "read-only"])
    live.add_argument("--network", action="store_true")
    live.add_argument("--worktree", default="worktree", choices=["worktree", "other"])
    live.add_argument("--no-auth", action="store_true")
    live.add_argument("--bad-config", action="store_true")
    live.add_argument("--interrupt", choices=["SIGINT", "SIGTERM"])
    live.add_argument("--timeout", type=int, default=120, choices=range(10, 181))
    live.add_argument("--codex", default="codex")
    args = parser.parse_args()
    if args.command == "prepare":
        prepare()
    elif args.command == "check":
        print(json.dumps(check(args.directory, HERE / "schemas" / (args.schema + ".json"))))
    else:
        run(args)


if __name__ == "__main__":
    main()
