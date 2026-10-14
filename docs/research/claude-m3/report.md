# Claude headless usage-limit evidence — M3

Updated: 2026-10-14. Scope: [#61](https://github.com/rcpassos/mergeyard/issues/61),
parent [M3 specification #60](https://github.com/rcpassos/mergeyard/issues/60).

**Release gate: observed weekly rejection captured.** On 2026-10-13, a bounded
headless call against the user's naturally limited account failed with native
`status: rejected`, assistant `error: rate_limit`, and a terminal error result.
The sanitized positive fixture is [weekly-rejected](fixtures/weekly-rejected/run.json).
This satisfies #61's missing live-evidence requirement for the observed variant;
binding the limit signal to production classification belongs to
[#81](https://github.com/rcpassos/mergeyard/issues/81). The successful warning and
authentication failure below are negative controls, not temporary limits.

## Observed successful warning — 2026-10-06

The invocation and supervisor behavior are preserved in
[fixtures/allowed-warning/run.json](fixtures/allowed-warning/run.json); selected
native records are in [events.jsonl](fixtures/allowed-warning/events.jsonl).
The capture used [probe.py](probe.py) exactly once, outside the execution sandbox
for authenticated network access. Prior Claude continuity probes were not rerun.

| Observation | Captured value |
|---|---|
| CLI version | `2.1.288 (Claude Code)`; init also reports `2.1.288` |
| UTC start | `2026-10-06T21:35:59.496258+00:00` |
| Model | Requested `sonnet`; init/assistant resolved to `claude-sonnet-5-5` |
| Prompt | `Reply exactly OK. Do not use tools.` |
| Native event order | `system/init`, `assistant`, `rate_limit_event`, `result` |
| Identity | Preassigned `edc578c3-97a0-46e8-aee9-14eb4304a7f8`; all four records carry that ID |
| Exit / elapsed / signals | `0` / `5.44` seconds / no watchdog signal |
| Assistant | Text `OK`; no top-level `error` or `api_error_status` |
| Limit status | `allowed_warning`, `rateLimitType: seven_day`, `utilization: 0.99`, `isUsingOverage: false` |
| Native completion | `subtype: success`, `is_error: false`, `terminal_reason: completed`, `api_error_status: null`, `stop_reason: end_turn`, `num_turns: 1` |
| Cost estimate / stderr | `total_cost_usd: 0.0049422`; empty stderr |

The session ID was available in startup and final records. Capture was to a
file, inspected after exit: this does not establish when a streaming consumer
could persist identity or whether this new session can resume after a limit.

The warning's reset source is `rate_limit_info.resetsAt: 1791367200`, also
present at `rate_limit_info.unifiedWindows.seven_day.resetsAt`. The five-hour
window reports `utilization: 0` and `resetsAt: 1791340200`. Interpreted as epoch
seconds, these are 2026-10-07 10:00:00Z and 02:30:00Z respectively. The numeric
values are observed; the units are an **inference** from their magnitude and
plausible future dates, consistent with the documented Unix timestamp below.
There was no independent textual reset or usage-screen cross-check. Rejected
event units/precedence remain unobserved. A warning's reset is not a reason to
pause a normally completed run.

The durable capture uses field selection, with retained values unchanged and
the private cwd replaced by a placeholder. All four event types are retained
in their original order. Host/discovery metadata, message/request/event IDs,
token breakdowns, and unrelated diagnostics are omitted, as recorded in
`run.json`. No credentials, account identifiers, or unrelated conversations
are included. The original temporary files are private working evidence,
not a durable release artifact.

## Observed weekly rejection — 2026-10-13

After the user reported an existing account limit, the same [probe.py](probe.py)
was invoked once. That first attempt hit an expired OAuth session; after the
user refreshed the login and explicitly authorized a retry, one more invocation
captured the weekly rejection. Neither call exhausted quota or retried
automatically. Both used an empty temporary workspace and the existing bounds,
outside the execution sandbox for subscription authentication/network access.
No ordinary continuity checks were repeated.

Exact invocation, version, timing, and process outcome are in
[weekly-rejected/run.json](fixtures/weekly-rejected/run.json); the four native
records, in order, are in [events.jsonl](fixtures/weekly-rejected/events.jsonl).

| Observation | Captured value |
|---|---|
| CLI version | `2.1.288 (Claude Code)`; init reports `2.1.288` |
| UTC start | `2026-10-13T12:02:05.527172+00:00` |
| Model | Requested `sonnet`; init resolved to `claude-sonnet-5-5`; error assistant model is `<synthetic>` |
| Prompt | `Reply exactly OK. Do not use tools.` |
| Native event order | `system/init`, `rate_limit_event`, `assistant`, `result` |
| Identity | Preassigned `46f8963c-6b42-491e-9924-85fad092ecad`; all four records carry that ID |
| Exit / elapsed / signals | `1` / `4.03` seconds / no watchdog signal |
| Limit signal | `rate_limit_info.status: rejected`, `rateLimitType: seven_day`, `isUsingOverage: false` |
| Assistant | Top-level `error: rate_limit`; text `You've hit your weekly limit · resets 11am (Europe/Lisbon)`; no assistant `api_error_status` |
| Native completion | `subtype: success`, `is_error: true`, `terminal_reason: api_error`, `api_error_status: 429`, `stop_reason: stop_sequence`, `num_turns: 1` |
| Result text | Same weekly-limit/reset text as assistant |
| Cost estimate / stderr | `total_cost_usd: 0`; empty stderr |

The result's `subtype: success` does **not** establish successful model
completion. This was a naturally rejected request with an error result and
nonzero exit, not a watchdog cancellation, budget exhaustion, or turn-bound
failure. Identity is available in init, limit, assistant, and final records.
Files were inspected after exit, so exact delivery timing to a streaming
consumer was not measured. Resumability was subsequently observed after reset
on 2026-10-14, as recorded below; the UUID alone was not treated as proof.

### Reset source, units, and accompanying overage fields

The top-level reset is `rate_limit_info.resetsAt: 1791972000`, equal to
`rate_limit_info.unifiedWindows.seven_day.resetsAt`, whose utilization is `1`.
Interpreting this as Unix epoch seconds gives **2026-10-14 10:00:00Z**, or
**2026-10-14 11:00:00 Europe/Lisbon**, consistent with the independently emitted
assistant/result reset text and the capture time. The units are inferred from
numeric magnitude, calendar plausibility, and this native text cross-check,
consistent with the documented SDK Unix timestamp. No usage-screen cross-check
was performed, and the text itself has no date. Epoch milliseconds would land
in January 1970 and would not agree with the native reset text/capture date.

The five-hour window has `utilization: 0.65` and `resetsAt: 1791896400`
(2026-10-13 13:00:00Z / 14:00 Europe/Lisbon). It is not the rejected window:
using the earliest reset across windows would resume before the observed weekly
restriction ends. The explicit top-level reset for the rejected `seven_day`
window is the reliable source in this fixture. Conflicting top-level/window
reset values and missing reset fields remain unobserved.

The same event includes `overageStatus: rejected` and
`overageDisabledReason: out_of_credits`. Preserve these unchanged: this capture
shows a temporary subscription weekly limit with unavailable overage. It does
not establish a standalone exhausted-credit block or require treating the
weekly reset as indefinite. A classifier must assess the rejected window,
assistant error, and terminal result together; neither an HTTP 429 alone nor
the overage reason alone is the observed temporary-limit contract.

### Authentication failure control

[authentication-failed/run.json](fixtures/authentication-failed/run.json) and
[events.jsonl](fixtures/authentication-failed/events.jsonl) preserve the first
2026-10-13 attempt. It started at `12:00:17.404377+00:00`, exited `1` after
`3.85` seconds without a watchdog signal, and emitted `system/ui_invalidate`,
`system/init`, `assistant`, `result`. All four carry the requested UUID
`ec11a963-e393-4071-96ac-299869650eac`.

The assistant has `error: authentication_failed` and the diagnostic
`Failed to authenticate: OAuth session expired and could not be refreshed`.
The result has `subtype: success`, `is_error: true`, `terminal_reason: api_error`,
`api_error_status: null`, `stop_reason: stop_sequence`, `num_turns: 1`, and
`total_cost_usd: 0`; stderr is empty. There is no `rate_limit_event` or reset
source. Exit 1, identity availability, and an error completion are therefore
insufficient to establish a usage limit. This is an ordinary authentication
failure control for offline classifier tests.

Both new fixtures use the same documented field-selection policy as the warning:
all native event types/order and retained values are preserved, the private cwd
is replaced, and host/discovery metadata, message/request/event IDs, and token
breakdowns are omitted. See each `run.json` for raw/retained event counts. Only
this probe's prompt and diagnostics are retained; credentials, account
identifiers, and unrelated conversations are excluded.

## Observed post-reset resume — 2026-10-14

The user authorized continuation of #81 after the recorded reset. One bounded
print-mode call resumed the exact weekly-rejected session from its original,
still-present private workspace. The capture reused [probe.py](probe.py)'s
supervisor and arguments with `--resume` replacing `--session-id`, the captured
UUID replacing the fresh UUID, and the original workspace replacing a new one.
No credentials or unrelated transcripts were read or copied. There was no
automatic retry and no new quota-exhaustion call.

The exact invocation and outcome are preserved in
[weekly-resumed/run.json](fixtures/weekly-resumed/run.json), with selected native
records in [events.jsonl](fixtures/weekly-resumed/events.jsonl).

| Observation | Captured value |
|---|---|
| CLI version | `2.1.288 (Claude Code)` |
| UTC start | `2026-10-14T12:07:16.670725+00:00`, after the weekly reset at `10:00:00Z` |
| Session | `--resume 46f8963c-6b42-491e-9924-85fad092ecad`; all four native records carry that same ID |
| Prompt / assistant / result | `Reply exactly OK. Do not use tools.` / `OK` / `OK` |
| Event order | `system/init`, `assistant`, `rate_limit_event`, `result` |
| Exit / elapsed / signals | `0` / `4.59` seconds / no watchdog signal |
| Limit status | `allowed`, `rateLimitType: five_hour`, `isUsingOverage: false` |
| Completion | `subtype: success`, `is_error: false`, `terminal_reason: completed`, `api_error_status: null`, `stop_reason: end_turn`, `num_turns: 1` |
| Cost estimate / stderr | `total_cost_usd: 0.005351`; empty stderr |

This establishes that the captured limited session is resumable after reset
under the reapplied probe settings. It does not establish other limit variants
or continuity of an engineering phase with `--json-schema`. The successful
control still includes `overageDisabledReason: out_of_credits`; that field alone
is not an exhausted-credit block. Selection/redaction follows the existing
fixture policy; the cwd placeholder denotes the original capture workspace.

Production classification now binds only the observed weekly rejection. Offline
tests replay the positive and negative fixtures and cover configured cooldown
and the existing bounded missing-session recovery if a resumed session later
becomes unavailable. Normal CI does not execute these live probes.

## Documentation-derived expectations

Current official documentation was queried through Context7 and checked against
the primary pages on 2026-10-13. These SDK and CLI expectations do not establish
binary behavior on a rejected request:

- The [Agent SDK rate-limit types](https://code.claude.com/docs/en/agent-sdk/python#ratelimitinfo)
  describe `allowed`, `allowed_warning` (approaching the limit), and `rejected`
  (limit reached). `resets_at` is an optional Unix timestamp; documented windows
  include five-hour, seven-day, model-specific seven-day, and overage limits.
  [RateLimitEvent](https://code.claude.com/docs/en/agent-sdk/python#ratelimitevent)
  carries `session_id` and supports warning or backoff on status changes. Python
  field names are SDK names; the successful CLI control above observed
  `rate_limit_info.resetsAt`, also observed in the rejected CLI capture above.
- The [Python result contract](https://code.claude.com/docs/en/agent-sdk/python#resultmessage)
  allows `subtype: success` with `is_error: true` when the final model request
  fails. `api_error_status` is the terminating HTTP status, or `None` without an
  API error; `terminal_reason` explains why the loop ended and may be absent on
  older or synthesized results. The
  [ResultError guidance](https://code.claude.com/docs/en/agent-sdk/python#resulterror)
  says to inspect `terminal_reason` before `subtype`. This is a documented
  expectation, not a captured temporary-limit failure contract.
- The [headless basic usage](https://code.claude.com/docs/en/headless#basic-usage)
  promises zero exit on success and nonzero on failure;
  [streaming output](https://code.claude.com/docs/en/headless#stream-responses)
  is newline-delimited JSON with a final `result`. Existing
  [M1 interruption evidence](../harness-spikes.md#q9--sigint-during-streamed-output-then-resume)
  nevertheless has an error result with process exit 0; preserve both native
  and process outcomes instead of relying on exit alone.
- [Earlier research §2.3](../harness-spikes.md#23-usage-limits) records interactive
  `rate_limit`/429 messages and wall-clock session/weekly reset strings, plus
  spend-cap variants. Those are prior interactive observations, not this
  ticket's missing headless failure contract.

## Unobserved variants and downstream requirement

Still unobserved: five-hour/model-specific rejections, absent/malformed or
conflicting reset data, text-only failure, `--json-schema` failure behavior,
standalone exhausted credits/spend caps, and transport throttling. None is a
claimed pass. The weekly-rejected fixture has no phase schema or
`structured_output`; it establishes the native failure contract, not a valid
engineering-phase result.

#81 should bind only the observed limit signal to the adapter-owned classifier
and test it against these offline positive and negative fixtures. Unknown
variants remain ordinary failures. The limited session's post-reset resume is
now observed above; do not make another request before reset merely to test
availability. A startup failure, budget/turn-bound
failure, forced fake error, or successful warning is not a positive limit signal.
The existing [Claude M1 continuity results](../harness-spikes.md#6-issue-9-live-m1-adapter-verification-2026-10-04)
remain the evidence for ordinary resume/interactive handback. The #81 adapter
binding uses this observed rejection and post-reset resume evidence.
