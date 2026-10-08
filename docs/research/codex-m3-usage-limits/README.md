# Manual Codex M3 usage-limit probe

Supports [issue #62](https://github.com/rcpassos/mergeyard/issues/62), with
[source expectations](sources.md), a [live report](report.md), and selected
[sanitized evidence](evidence.json). This probe does not implement runtime
classification or repeat M2 phase, skill, or conversation checks.

The required headless limit failure is **unverified**. The one live request
completed normally. Passing offline tests does not satisfy that release gate.

## Explicit live capture

Use an already usage-limited ChatGPT-backed account; do not deliberately burn
quota to reach a limit. Requirements: Python 3.9+, Git, the authenticated Codex
CLI with file-based `auth.json`, and outbound access to the Codex service. The
recorded version is 0.156.1. No dependencies are installed.

Select a new private directory that does not exist:

```sh
TZ=Europe/Lisbon PYTHONDONTWRITEBYTECODE=1 \
python3 docs/research/codex-m3-usage-limits/probe.py run --live \
  --root /private/tmp/mergeyard-codex-m3-your-new-case --timeout 30
```

`--live` explicitly acknowledges authenticated model requests and quota use.
Each root accepts exactly one CLI invocation; reuse fails before launching.
There is no automatic retry or quota-exhaustion loop in this tool. Codex can
retry transport failures internally. The watchdog defaults to 30 seconds
(configurable 10–60), followed by five seconds for process-group cleanup. There
is no CLI dollar budget. `--model` selects the request model; the default
`gpt-6-luna` was supported in prior M2 evidence and in this capture.

The probe creates an empty Git workspace and uses read-only sandboxing, low
effort, network-disabled tools, no user config/rules/plugins/web search, and the
existing strict implement schema for a minimal availability response. The
trusted prompt asks the model to read a probe input file. That input requests no
further tools, file reads, or engineering work. The observed model also ran
`pwd` and `git status`; read the native events before claiming tool-free execution.
Personal skill discovery may still contribute context; no skills are requested.
This is a first-turn probe, not an exact-ID resume or reset-recovery check.

The existing [M2 supervisor](../codex-m2/probe.py) handles the watchdog, owned
process group, early `thread.started` identity capture, and authentication
staging/removal. Only `auth.json` and available model metadata are copied into
a private `CODEX_HOME`. The original authentication is untouched. SIGTERM to
the supervisor records cancellation and cleans up the child and auth copy.
SIGKILL can prevent cleanup: remove only the private fixture's
`codex-home/auth.json` before sharing or deleting it.

Raw `command.json`, `events.jsonl`, `stderr.log`, `last-message.json`,
`identity.json`, and `run.json` are saved under `ROOT/evidence/usage-limit`.
`ROOT/environment.json` records version, model, wall clock/offset, `TZ`, input,
and watchdog. The matching session rollout remains private under
`ROOT/codex-home/sessions`. `rollout-evidence.json` exports only token-count
rate-limit snapshots, structured failure fields, and an assistant-message count.
It requires one matching rollout filename and matching session metadata ID;
unrelated sessions cannot supply evidence. Malformed JSON fails capture/checking.
Always inspect diagnostics manually before publishing. Do not publish auth,
full rollouts, model caches, or user config.

## Offline checking

```sh
python3 docs/research/codex-m3-usage-limits/probe.py check \
  /private/tmp/mergeyard-codex-m3-your-new-case/evidence/usage-limit
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s docs/research/codex-m3-usage-limits -p '*_test.py' -v
```

The checker is a research aid, not a production classifier. It requires exit 1,
no supervisor interruption, no `turn.completed`, and a `turn.failed` message
containing the captured/source-expected usage-limit phrase. Structured reset
candidates come only from the same session's last token-count snapshot and an
associated `usage_limit_exceeded` diagnostic; windows must report at least 100%
usage and positive epoch seconds. All exhausted-window candidates are retained;
choosing the applicable wake time belongs to later adapter work. It never
converts message times or invents cooldown timestamps. `message_only` means a
retry-at string exists, not that it has been parsed or verified.

`first_request_limited` is evidence of one failed native turn before any
assistant response in its matching rollout or native item in stdout. It cannot rule out internal
transport retries or prove server-side request counts. Missing rollout evidence
keeps this false. First-request limit behavior remains unverified live.

The [fixture manifest](fixtures/manifest.json) labels provenance explicitly:
`observed-available-0.156.1` is the real successful request, while both failure
fixtures are **synthetic, unverified variants** based on source/prior interactive
evidence. They exercise extraction offline and must never be cited as observed
headless failures. The fake CLI regression test verifies one launch, reset
extraction, auth cleanup, sanitization and refusal to reuse a capture directory.
The ordinary `make test`/CI path runs these offline checks only.
