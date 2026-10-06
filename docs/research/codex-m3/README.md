# Manual Codex M3 continuity probe

This is the opt-in live reproduction for
[issue #63](https://github.com/rcpassos/mergeyard/issues/63).
See [report.md](report.md), [evidence.json](evidence.json), and
[approval-terminal.txt](approval-terminal.txt) for the observed run.
It reuses the [M2 fixtures and supervisor](../codex-m2/README.md).
No live command is called by normal tests, CI, or Mergeyard.

Requirements: Python 3.9+, Git, an authenticated Codex CLI with file-based
`auth.json`, macOS `script` for this recording command, and an interactive
terminal. The observed version was Codex 0.156.1. `--live` acknowledges model
calls that consume account quota. No dependencies are installed. Keep stages
sequential, inspect results, and do not retry automatically.

## 1. Prepare and interrupt headless execution

From the repository root:

```sh
python3 docs/research/codex-m2/probe.py prepare
# Assign the printed private directory to PROBE_ROOT.
python3 docs/research/codex-m2/probe.py run --live --root "$PROBE_ROOT" \
  --case sigint --input interrupt --interrupt SIGINT
```

Set `SESSION_ID` to `Captured live identity`. Confirm the supervisor has
returned, `evidence/sigint/run.json` records SIGINT and a finished exit code,
and the CLI is no longer running before continuing. The fixture input holding
the seed marker is removed by the supervisor. Exit 1 and missing native
completion were observed; never interpret the interrupted turn as success.

## 2. Resume the exact conversation interactively

```sh
/usr/bin/script -q "$PROBE_ROOT/evidence/interactive.terminal.log" \
  python3 docs/research/codex-m3/interactive.py --live \
  --root "$PROBE_ROOT" --session "$SESSION_ID"
```

The wrapper requires a terminal and the completed SIGINT record for that exact
ID. It refuses to overwrite `evidence/interactive`, shares the fixture's
16-invocation cap, and has a 180-second watchdog. It stages authentication
only in the private fixture home and removes it on normal exit, timeout, or
SIGTERM. It uses ordinary `workspace-write`/`on-request` permissions with a
user approval reviewer and disables plugins/web search. It excludes temporary
folders from automatic writable roots so the permission probe cannot silently
pass because of its temporary location. If `TERM=dumb` warns, answer `y`, as
in the observed run, or start with a suitable terminal type.

Introduce a new, arbitrary marker directly in the TUI, never in a prompt file:

> Remember this new conversation-only marker: YOUR_NEW_MARKER. Never write it
> in a file or shell command. Recover the earlier marker from the interrupted
> turn and acknowledge both. Exercise normal interactive permission handling:
> request `sandbox_permissions=require_escalated` to run
> `printf 'permission-approved\n' > ABSOLUTE_PROBE_ROOT/approval.txt`, with a
> justification requesting permission to create this harmless file outside the
> worktree. Wait for the operator. Do not commit or change worktree files.
> Then reply briefly in prose.

Replace `ABSOLUTE_PROBE_ROOT` with the actual fixture path. Inspect the tool
request and approve only that harmless write using one-time “Yes, proceed.”
If the model declines without attempting a tool, inspect `/permissions` and
record the refusal. The observed follow-up was:

> The interactive /permissions menu currently shows Ask for approval. Please
> attempt the harmless approval.txt command with
> sandbox_permissions=require_escalated through the tool so we can observe its
> actual response. Do not infer the current policy from the previous headless
> turn. Keep the conversation markers private to conversation. No other changes.

Inspect the final response, run `/exit`, and wait for the wrapper to return.
Require exit 0 and `signal_sent: null` in `evidence/interactive/run.json`.
If a watchdog/cancellation occurs, retain it as failed evidence and do not
claim a successful interactive exit. Never resume concurrently, clear, branch,
or select another conversation.

## 3. Hand back to headless execution

Only after the interactive process has exited, replace the external handback
input without repeating either conversation marker:

```sh
cat > "$PROBE_ROOT/inputs/restart.md" <<'INPUT'
# Headless handback
Return the native implement result with status success. Include the exact conversation-only marker introduced during our interactive terminal turn, and the earlier marker from the interrupted headless turn, in summary. Do not inspect terminal logs, sessions, input history, or other evidence files. No shell commands are needed. Keep worktree files unchanged.
INPUT
python3 docs/research/codex-m2/probe.py run --live --root "$PROBE_ROOT" \
  --case sigint-resume --input restart --resume "$SESSION_ID" \
  --skill m2-amber --network
```

This explicitly reapplies model `gpt-6-luna`, effort `low`, headless approval
`never`, sandbox `workspace-write`, network access, skill, native implement
schema, JSONL output, and a fresh last-message output path. Select a supported
model explicitly with `--model` on headless runs and the interactive wrapper
if reproducing on another account; record that difference from the observed
run. Do not substitute another identity.

## 4. Check and preserve evidence

The existing offline checker makes no model request:

```sh
python3 docs/research/codex-m2/probe.py check \
  "$PROBE_ROOT/evidence/sigint-resume" --schema implement
```

Require `native_success: true` and `schema_valid: true`. Also compare the actual
final summary to the independently chosen interactive marker, verify the
`thread.started.thread_id` equals `SESSION_ID`, and inspect every command in
the final JSONL. A result file or exit 0 alone is insufficient. Reject a run
that reads evidence files to recover the marker. Verify the seed input is gone,
the manual marker is absent from worktree files and the final input, and
`git -C "$PROBE_ROOT/worktree" status --porcelain` is empty.

Inspect the fixture's `codex-home/sessions` rollout for effective `turn_context`
model, effort, cwd, approval/reviewer, and sandbox values across all turns.
Preserve sanitized exact argv, process exit/signal records, native completion
and result, interactive input, approval menu, and relevant rollout records.
Replace private roots with placeholders; omit credentials, account/quota
metadata, personal skill catalogs, and reasoning. Never publish the entire
home or raw transcript without inspecting it. Unverified or failed behavior
remains an explicit release dependency, as recorded in [report.md](report.md).

SIGKILL of a supervisor can prevent cleanup. Before sharing or deleting the
fixture, verify its processes are stopped and remove only the staged
`$PROBE_ROOT/codex-home/auth.json` copy if present. After preserving evidence,
delete the disposable fixture directory; it contains a separate repository,
not a worktree of the product repository. The wrapper shares the M2 helpers;
its later cancellation/cleanup hardening does not change the recorded CLI argv
or imply that those exceptional shutdown paths were live-tested.
