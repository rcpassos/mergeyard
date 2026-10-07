# Claude headless usage-limit evidence — M3

Date: 2026-10-06. Scope: [#61](https://github.com/rcpassos/mergeyard/issues/61),
parent [M3 specification #60](https://github.com/rcpassos/mergeyard/issues/60).

**Release gate: unresolved.** One bounded live call completed normally. No real
headless usage-limit failure was captured. Do not ship a Claude temporary-limit
classifier on the strength of this warning, interactive transcripts, or SDK
documentation alone. Issue #61 remains incomplete until a genuine headless
failure is observed and preserved; no positive failure fixture is fabricated.

## Observed live facts

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

## Documentation-derived expectations

Current official documentation was queried through Context7 on 2026-10-06.
These expectations do not establish binary behavior on a rejected request:

- The [Agent SDK rate-limit types](https://code.claude.com/docs/en/agent-sdk/python#ratelimitinfo)
  describe `allowed`, `allowed_warning`, and `rejected`; `resets_at` is a Unix
  timestamp. The Python field names are not raw CLI field names. The observed
  CLI uses `rate_limit_info.resetsAt` (camel case for the inner field).
- The [TypeScript result contract](https://code.claude.com/docs/en/agent-sdk/typescript#sdkresultmessage)
  describes `api_error_status` as the HTTP status terminating the conversation,
  absent/null without an API error. Do not guess rejected result subtype,
  terminal reason, or field presence from the successful control.
- The [headless documentation](https://code.claude.com/docs/en/headless)
  describes JSONL streaming and nonzero exits on failure. Existing
  [M1 interruption evidence](../harness-spikes.md#q9--sigint-during-streamed-output-then-resume)
  nevertheless has an error result with process exit 0; preserve both native
  and process outcomes instead of relying on exit alone.
- [Earlier research §2.3](../harness-spikes.md#23-usage-limits) records interactive
  `rate_limit`/429 messages and wall-clock session/weekly reset strings, plus
  spend-cap variants. Those are prior interactive observations, not this
  ticket's missing headless failure contract.

## Unobserved variants and completion requirement

Still missing: a real `-p` temporary-limit failure, its terminal result or
absence, exit code, assistant/native error fields, whether `status: rejected`
is emitted, identity availability on that failure, and reliable reset source
and units. Session/weekly/model-specific limits, absent/malformed reset data,
text-only failure, `--json-schema` failure behavior, exhausted credits/spend
caps, and transport throttling were not exercised. None is a claimed pass.

When an account is naturally restricted, run one explicit bounded capture for
the affected model, following [README.md](README.md). Preserve sanitized native
failure records and metadata, compare reset sources, and add a separate positive
offline fixture before resolving the gate. A startup failure, budget/turn-bound
failure, forced fake error, or successful warning cannot satisfy that step.
The existing [Claude M1 continuity results](../harness-spikes.md#6-issue-9-live-m1-adapter-verification-2026-10-04)
remain the evidence for ordinary resume/interactive handback. This work changes
research evidence only; production classification belongs to the dependent
M3 adapter work.
