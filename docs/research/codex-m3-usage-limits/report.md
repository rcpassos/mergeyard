# Issue #62: Codex headless usage-limit evidence

Date: 2026-10-06. Scope: [issue #62](https://github.com/rcpassos/mergeyard/issues/62)
and [M3 specification #60](https://github.com/rcpassos/mergeyard/issues/60).

**The required real headless usage-limit failure remains unverified.** One
explicit bounded request completed successfully; no quota-exhaustion loop or
second live attempt was made. This remains an unresolved M3 release dependency,
as specified by the parent. Offline fixtures establish tooling behavior only.

## Observed live request

[Evidence](evidence.json) records `/opt/homebrew/bin/codex`,
`codex-cli 0.156.1`, the exact argv, isolated `CODEX_HOME`, selected native
records, stderr, exit and identity metadata, and matching rollout snapshots.
The request began at 2026-10-06 22:37:49 Europe/Lisbon (UTC+01:00), used
`gpt-6-luna` at low effort, and exited 0 after 9.29 seconds with no signal.

Invocation, with `ROOT` standing for the private fixture path recorded in the
JSON evidence:

```sh
codex exec --ignore-user-config --ignore-rules --json \
  -C "$ROOT/worktree" -s read-only -m gpt-6-luna \
  -c model_reasoning_effort=low \
  -c sandbox_workspace_write.network_access=false \
  -c features.plugins=false -c 'web_search="disabled"' \
  --output-schema docs/research/codex-m2/schemas/implement.json \
  -o "$ROOT/evidence/usage-limit/last-message.json" \
  " Read $ROOT/inputs/availability.md and follow it. Do not commit."
```

The actual schema argument was an absolute path, retained in `evidence.json`.
The model read that input, ran `pwd` and `git status`, then returned
`{"schema_version":1,"status":"success","summary":"availability probe"}`.
No engineering edits were observed. Native events were `thread.started`,
`turn.started`, agent-message/tool items, and `turn.completed`. There were no
`error` or `turn.failed` events. The identity
`01a11326-c64e-7011-91c1-1e54d8b96f0e` was captured while the process was live;
its matching rollout existed after exit. Staged authentication was removed.

The native completion reported 23,659 input tokens, 11,008 cached input tokens,
and 118 output tokens. The availability input is short, but CLI context still
contributes usage. These are telemetry, not a monetary bill.

The rollout's two token-count records both reported `limit_id: "codex"`,
primary usage 14%, a 10,080-minute window, `resets_at: 1791918668`, and no
secondary window. This is observed reset telemetry for an **unexhausted** window,
not an observed usage-limit wake time. The checker therefore supplies no reset
candidate. The same snapshot reported `credits.has_credits: false` and
`balance: "0"` despite a normally completed response: those fields alone do not
establish an exhausted-credit block.

## Contract status

| Required evidence | Status | Basis |
|---|---|---|
| Real headless limit failure, native error/completion records and exit | Unverified | The only live request completed normally. |
| Session identity and rollout when the first request is limited | Unverified | Identity/rollout were observed only on a successful first turn. |
| Structured reset epoch on a limited first request | Unverified | Unexhausted-window telemetry exists; backend rejection/reset delivery was not observed. |
| Missing reset and local-time message variants | Unverified live | Source expectations and explicitly synthetic offline cases only. |
| Sanitized durable evidence and offline fixtures | Available | Live completion plus labelled synthetic failures; no auth or full rollout retained in the repo. |
| Explicit bounded live execution, separate from normal CI | Available | One invocation, 30-second watchdog, inherited group/auth cleanup; CI uses a fake executable. |

[Pinned source findings](sources.md) establish the 0.156.1 expectation:
`exec --json` failure records expose messages rather than structured reset
fields; failed turns exit 1; usage-limit messages use machine-local retry times;
a rate-limit snapshot may be written before a sampling usage-limit error when
the backend supplies it; persisted native turn completion can retain
`usage_limit_exceeded`. These expectations do not turn synthetic fixtures into
live evidence. The prior [harness spike](../harness-spikes.md#33-usage-limits)
observed interactive/Desktop limit records, not this required headless case.

## Follow-up to close the release gate

Run the [single-request recipe](README.md) when the authenticated account is
already limited. Preserve exact version/argv, stdout/stderr, exit, identity
availability and the matching rollout's ordered failure/reset records. State
whether token-count evidence exists before the first failed turn and whether
any exhausted window supplies a reset. Missing evidence must stay missing.
A parsed native message time is a reported source; a configured 30-minute
cooldown is an assumption, never an observed reset. No cooldown was applied by
this research tool. Do not reuse unrelated sessions' snapshots.

A limit encountered after partial work, same-day/date-bearing messages,
per-model limits, missing/unreadable rollouts, multiple exhausted windows,
credit/spend-cap errors, and post-reset resume remain unverified variants. The
last item is follow-on recovery evidence, not a reason to repeat M2 contracts.
Keep issue #62 open until the required real failure is captured and reviewed.
