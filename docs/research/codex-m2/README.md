# Manual Codex M2 probes

These fixtures support [issue #37](https://github.com/rcpassos/mergeyard/issues/37).
Live probes are research tools, excluded from `go test`, CI, and application execution.
The offline supervisor regression check runs through `make test` and CI.
`run --live` makes authenticated model requests and can consume subscription
quota. Select one case at a time; inspect its evidence before proceeding.

Requirements: Python 3.9+, Git, Go 1.24+, an authenticated Codex CLI with file-based
`auth.json`, and outbound access to the Codex service. The recorded run used
Codex 0.156.1 on macOS. No dependencies are installed by the fixtures.

```sh
python3 docs/research/codex-m2/probe.py prepare
# Set PROBE_ROOT to the printed absolute path.
python3 docs/research/codex-m2/probe.py run --live --root "$PROBE_ROOT" \
  --case implement --input implement --skill m2-amber
```

`prepare` creates a private temporary repository, two linked worktrees, skills,
and phase inputs outside both worktrees. Each `run` copies only authentication
and model metadata into the fixture's private `CODEX_HOME`. It removes that auth
copy on exit, preserving sessions for explicit resume. User configuration, rules,
plugins, and web search are disabled; personal skill discovery may still occur,
but only the named fixture skills are requested. Inspect raw evidence before
sharing; never publish credentials or an entire home directory. Sending SIGTERM
to the Python supervisor enters cleanup: it stops the owned CLI process group,
removes staged authentication, saves `run.json` with
`signal_sent: "supervisor_SIGTERM"` and the child's exit code, and exits 143.
The handler latches the signal so process creation finishes before cleanup.
After child shutdown and authentication removal, final metadata publication
ignores further SIGTERM requests so the saved cancellation state stays consistent.
An external
SIGKILL of the Python supervisor can prevent credential cleanup: remove only
`$PROBE_ROOT/codex-home/auth.json` before sharing or deleting the fixture.

Every invocation has a 120-second watchdog (configurable from 10 to 180 seconds),
then SIGTERM and a five-second SIGKILL grace period. A fixture allows at most 16
invocations, refuses to overwrite a case directory, and never retries a model
request automatically. No CLI dollar budget was available. The interruption
cases signal the process group upon the first `sleep 30` command-start event;
they are blocked if that command never starts. The original implement/interruption
input is removed after its run, so a later turn must recover its marker from
conversation history. Keep sessions sequential to avoid interleaved turns and
authentication cleanup races.

Use the ID from `Captured live identity` / `identity.json`, never `--last`.
Apply these options to the common `run --live --root "$PROBE_ROOT"` command:

| Case | Options after the common command |
|---|---|
| Review, two skills | `--case review --input review --schema review --skill m2-amber --skill m2-blue --network` |
| Fix, exact implementer | `--case fix --input fix --schema fix --resume "$IMPL_ID" --effort medium --skill m2-amber --network` |
| Model override | Add `--model <model supported by your account>` to fix; the recorded `gpt-6.1-sol` attempt failed. Use `--case fix-retry` for one explicit retry. |
| Resumed review, two skills | `--case review-resume --input review --schema review --resume "$REVIEW_ID" --effort medium --skill m2-amber --skill m2-blue --network` |
| Resume settings override | `--case settings --input settings --resume "$IMPL_ID" --sandbox read-only --worktree other` |
| Unknown ID / full fresh input | `--case unknown --input unknown --resume 00000000-0000-4000-8000-000000000037` |
| Explicit fresh recovery | `--case fresh --input unknown` (only after inspecting the unknown-ID diagnostic) |
| Configuration failure | `--case bad-config --input unknown --bad-config` |
| No-auth control | `--case no-auth --input unknown --no-auth --timeout 25` |
| Non-strict schema | `--case bad-schema --input unknown --schema invalid` |
| Cache/network | `--case cache-network --input cache-network --network` |
| SIGINT | `--case sigint --input interrupt --interrupt SIGINT` |
| Restart after SIGINT | `--case sigint-resume --input restart --resume "$STOPPED_ID"` |
| SIGTERM | Restore `inputs/interrupt.md` from this directory, then `--case sigterm --input interrupt --interrupt SIGTERM` |
| Restart after SIGTERM | `--case sigterm-resume --input restart --resume "$STOPPED_ID"` |

Record `codex --version`, `codex exec --help`, `codex exec resume --help`, and
`codex login status` before probing. `command.json` records exact argv and
`CODEX_HOME`; `events.jsonl`, `stderr.log`, `last-message.json`, `identity.json`,
and `run.json` retain outputs, early identity, signals, exit code, and elapsed
time. In the private `codex-home/sessions` rollouts, inspect `turn_context` for
effective settings and user `response_item` entries containing `<skill>` for
explicit injection. Result markers additionally demonstrate following the body.
Skill markers do not appear in the trusted prompt or phase inputs.

Offline artifact checking makes no model call:

```sh
python3 docs/research/codex-m2/probe.py check "$PROBE_ROOT/evidence/fix" --schema fix
```

Run the supervisor regression check without a Codex login or model calls:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s docs/research/codex-m2 -p '*_test.py' -v
```

It starts a local fake CLI with dummy credentials, sends SIGTERM only to the
supervisor, and checks process cleanup, credential removal, and saved failure
metadata. The fake supplies valid result JSON and a completion event while
remaining alive; the checker must still reject that interrupted run as success.

The checker validates the exact fixture schema subset and requires exit 0,
`turn.completed`, no `error`/`turn.failed`, no supervisor interruption, and valid
final JSON. It is an evidence aid, not the production harness adapter. A valid
`blocked`/`failed` phase result is native completion, not application success.
Malformed/truncated JSONL raises an error rather than reporting success. Compare
requested/observed identities separately: a fresh thread can complete after an
unknown-ID resume. Tool failures can coexist with a completed model turn; inspect
their outputs before claiming a test, edit, or permission check passed.

The implement schema matches `harness.ImplementSchema()`; review and fix schemas
follow PRD §13.2, including strict nested objects and nullable finding locations.
Changing these contracts requires updating these fixtures deliberately.
