# Codex headless usage-limit evidence — M3

Updated: 2026-10-16. Scope: [#62](https://github.com/rcpassos/mergeyard/issues/62),
parent [M3 specification #60](https://github.com/rcpassos/mergeyard/issues/60).

**Release gate: observed first-request usage limit captured.** On 2026-10-16,
after the operator reported that the account had reached its Codex five-hour
limit, one bounded `codex exec --json` call failed on its first model request
with `error` + `turn.failed`, process exit 1, and the native rollout code
`usage_limit_exceeded`. The sanitized positive fixture is
[usage-limit-first-request](fixtures/usage-limit-first-request/run.json).
Binding this signal to production classification belongs to
[#82](https://github.com/rcpassos/mergeyard/issues/82).

## Observed first-request limit — 2026-10-16

The capture used [limit_probe.py](limit_probe.py) exactly once, outside the
execution sandbox for authenticated network access. It staged only `auth.json`
into a fresh private `CODEX_HOME`, removed it after exit, and kept the session
persisted (no `--ephemeral`) so rollout evidence could be inspected. No
automatic retry, wait-until-reset, or quota-exhaustion loop was used, and no
M2 phase-contract or M3 continuity check was repeated.

Exact argv, version, timing, and process outcome are in
[run.json](fixtures/usage-limit-first-request/run.json). All four exec JSONL
records are in [events.jsonl](fixtures/usage-limit-first-request/events.jsonl);
selected rollout records are in [rollout.jsonl](fixtures/usage-limit-first-request/rollout.jsonl).

| Observation | Captured value |
|---|---|
| CLI version | `codex-cli 0.156.1`; rollout `cli_version: 0.156.1`, `originator: codex_exec` |
| UTC start | `2026-10-16T12:23:30.455533+00:00` |
| Model / effort / sandbox | `gpt-6-luna` / `low` / `read-only`, approval `never`, from `turn_context` |
| Contract | Production-shaped `exec --json` with the M2 native implement `--output-schema` and `-o` |
| Exec event order | `thread.started`, `turn.started`, `error`, `turn.failed` |
| Identity | `thread.started.thread_id: 01a144aa-e286-7420-b07e-1fdbe4dca033`; rollout `session_meta.id` matches |
| Exit / elapsed / signals | `1` / `5.3` seconds / no watchdog signal |
| Exec error text | `error.message` and `turn.failed.error.message` are identical (below) |
| Rollout error | `task_complete.error.codex_error_info: usage_limit_exceeded`, `last_agent_message: null` |
| Rollout rate limits | `limit_id: premium`, `primary: null`, `secondary: null`, `rate_limit_reached_type: null`, credits `has_credits: false`, `balance: "0"` |
| Last message / stderr | `-o` file **not written**; stderr `Reading additional input from stdin...` |

The native message, with U+2019 apostrophe, was:

> You’ve hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro),
> visit https://chatgpt.com/codex/settings/usage to purchase more credits or try
> again at 1:52 PM.

This matches the Plus-plan template in
[harness-spikes §3.3](../harness-spikes.md#33-usage-limits). It is a natural
rejection with nonzero exit, not a watchdog cancellation, authentication,
configuration, schema, or unsupported-model failure. The exec JSONL carries
**only the message string**: there is no error code, status, window, or reset
epoch in `--json` output. `codex_error_info` exists only in the rollout.

### Session evidence when the first request is limited

Identity was available: `thread.started` was the first record, before
`turn.started` and the error. The probe captured stdout to a file and read it
after exit, so this run does not itself measure live delivery timing; the M2
supervisor previously observed `thread.started` before exit on every case that
emitted it ([codex-m2 evidence](../codex-m2/evidence.json)).

The limited request left a persisted rollout for that thread containing
`session_meta`, `turn_context`, the user prompt, `token_count`, and the failed
`task_complete`. No assistant message was recorded. Whether this thread can be
resumed after reset was **not** tested here; a rollout file or thread ID alone
does not prove resumability.

### Reset sources, units, and fallback

Observed reset sources for this failure:

1. **Textual reset in the native message** — `try again at 1:52 PM`. The format
   is the same-day 12-hour form with no date, seconds, or time zone. The rollout
   `turn_context.timezone` is `Europe/Lisbon`, the host zone; 1:52 PM there on
   the capture date is **2026-10-16 12:52:00Z**, 28.5 minutes after the request.
2. **No structured reset for this thread.** The only `token_count.rate_limits`
   snapshot was the `premium` bucket with `primary`/`secondary: null`. The
   earlier research expectation that a first-request limit writes
   `rate_limits.primary.resets_at` to the rollout (§4 question 2) is **not
   observed**: the record is written, but carries no window or reset.

**Operator-local cross-check (not a fixture).** The operator's own Codex Desktop
sessions on the same account (`0.162.0-alpha.17.2`) reported, at
`12:19:45.856Z`, `limit_id: codex`, five-hour primary window
(`window_minutes: 300`) at `used_percent: 100.0` with `resets_at: 1792155130`,
and the weekly secondary at 34% (`resets_at: 1792680939`). As epoch seconds,
`1792155130` is **2026-10-16 12:52:10Z / 13:52:10 Europe/Lisbon**, agreeing with
the probe's text to the minute; the weekly value is 2026-10-22 14:55:39Z. Those
Desktop sessions then emitted the same `premium`/null snapshot immediately before
their own `usage_limit_exceeded` failures. So the structured five-hour reset was
visible only in a session that had an earlier successful response; it was not
emitted to the headless thread that was limited on its first request. Only the
selected numeric rate-limit fields were read from those private sessions.

Consequences for #82:

- The textual time is minute-truncated (10 seconds earlier than the structured
  value here). A parser must add margin rather than resume at the exact minute.
- Converting it requires the CLI host's local time zone and the capture time.
  The cross-day form (`Oct 3rd, 2026 6:23 PM`, per §3.3) and `try again later.`
  (no reset) were not observed in this capture.
- The operator called this the five-hour limit, and the Desktop cross-check
  agrees, but the headless failure itself names no window. A classifier must not
  claim which window was exhausted from this output alone.
- `has_credits: false` / `balance: "0"` accompanies an ordinary temporary
  subscription limit, as Claude's `out_of_credits` overage field did in #61. It
  does not establish a standalone exhausted-credit block; `Your workspace is out
  of credits` and spend-cap variants remain unobserved and non-time-bound.
- When neither text nor rollout yields a reset, the configured cooldown is the
  fallback. That fallback is an assumption, not an observed reset.

### Sanitization

All exec JSONL records are retained verbatim and in order. The rollout keeps 6
of 12 records by field selection with native values unchanged: session meta,
task start, selected turn context, the probe's user prompt, the rate-limit
snapshot, and the failed completion. Omitted: developer/skill/multi-agent
instructions, environment context, world state, item echo, message IDs, runtime
roots, permission profile, and collaboration metadata. Private paths are
replaced with `<private temporary workspace>`, `<private temporary root>`, and
`<repository>`. Each retained rollout record also carries a curator-added,
non-native top-level `ordinal`: its zero-based line index in the raw rollout.
Native records have only `timestamp`, `type`, and `payload`; replay tests must
not treat `ordinal` as part of the Codex format. No credentials, account
identifiers, or unrelated content are included. The raw temporary directory and Desktop sessions are not published.

## Negative controls

No new negative controls were spent. The existing
[Codex M2 evidence](../codex-m2/evidence.json) already preserves ordinary
`exec --json` failures with exit 1 and `error`/`turn.failed`: unsupported model
(`fix`), invalid reasoning effort (`bad-config`), missing authentication with
reconnect errors (`no-auth`), and non-strict schema (`bad-schema`), plus
SIGINT/SIGTERM interruptions. The unknown resume ID (`unknown`) differs: exit 1
with **no JSONL events** and no identity, only a stderr
`thread/resume failed: no rollout found` diagnostic. That is the missing-session
shape #82 must keep apart from a usage limit. None contains
`hit your usage limit`. Together with this fixture they show that exit 1,
`turn.failed`, or a thread ID alone is not a usage-limit signal.

## Unobserved variants and downstream requirement

Still unobserved: weekly-window and per-model (`for <limit_name>`) limits, the
cross-day reset format, `try again later.` without a reset, workspace credit
exhaustion and spend caps, `rate_limit_exceeded` throttling, a limit reached
mid-turn after tool calls, a limit on `exec resume`, the ASCII-apostrophe
variant, other plan templates, and post-reset resumability of the limited
thread. None is a claimed pass.

#82 should bind only the observed signal: an exec `error`/`turn.failed` message
matching `hit your usage limit`, corroborated where available by rollout
`codex_error_info: usage_limit_exceeded`. Derive the reset from the native text
with the host time zone and a safety margin, or from a rollout
`rate_limits.*.resets_at` if one is present for that thread; otherwise use the
configured cooldown and record that it is a fallback. Persist `thread.started`
identity as soon as it arrives: it was available before the first-request limit.
Unknown variants stay ordinary failures. Normal CI must replay these fixtures
offline and never call the live probe.
