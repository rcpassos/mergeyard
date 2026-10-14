# Manual Claude M3 usage-limit capture

Evidence for [issue #61](https://github.com/rcpassos/mergeyard/issues/61).
Read [report.md](report.md) before using the fixtures: a real headless weekly
rejection was captured on 2026-10-13. The positive fixture establishes only that
observed variant; the successful warning and authentication failure are negative
controls. Production classifier work is tracked separately in
[#81](https://github.com/rcpassos/mergeyard/issues/81).

Run only when explicitly investigating an already suspected account limit.
This performs one authenticated model invocation and can consume quota:

```sh
python3 docs/research/claude-m3/probe.py --live
```

Requirements: Python 3.9+, an authenticated Claude CLI supporting the recorded
flags, and outbound access to Claude. No dependencies are installed. The recorded
binary was 2.1.288. `--claude /absolute/path/to/claude` selects a binary;
`--model <model>` selects the affected model (default `sonnet`). A model alias
can resolve differently on a later run; record `system/init.model` too.

The script creates a private temporary directory and prints its absolute path.
It captures version, exact argv, UTC start time, requested session UUID,
exit code, elapsed time, watchdog signals, stdout JSONL, and stderr. The child
uses an empty workspace, no stdin, no ordinary tools, no hooks or external MCP,
empty setting sources, and safe mode. Existing subscription authentication stays
in its normal location; no credentials or existing conversations are copied.
The session is new and persisted; `--bare` is avoided because it bypasses
subscription authentication. Native output may still include host metadata.

Each run invokes print mode once, with one allowed turn, a USD 0.10 CLI budget,
and a 90-second watchdog followed by SIGTERM and a five-second SIGKILL grace
period. The CLI budget is an additional bound, not an exact billing guarantee.
The supervisor stops remaining descendants on exit. Do not kill the supervisor
with SIGTERM/SIGKILL: that can bypass its cleanup and watchdog. There is no
automatic retry, wait-until-reset, or quota-exhaustion loop. If the request
completes, stop; another invocation requires a deliberate decision based on
new evidence of an existing restriction. Never repeat successful requests to
manufacture a failure.

Inspect raw evidence privately before sharing. Preserve only relevant native
records with a documented field-selection/redaction policy. Remove credentials,
account identifiers, host paths, discovery lists, request IDs, and unrelated
content. Keep retained diagnostic values unchanged and session identity
consistent across records. Commit selected evidence, never the raw temporary
directory or a Claude configuration/transcript directory.

For a genuine failure, record the observed exit code and any watchdog signal,
assistant error fields/text, every `rate_limit_event`, and the terminal `result`
(or its absence). Compare the requested UUID with all emitted session IDs;
identity availability does not by itself prove resumability. For reset values,
record the exact native field, value, units and basis for those units, and
compare any textual wall-clock time/time zone. Keep absent fields absent.
Distinguish temporary limits from exhausted credits/spend caps, ordinary 429
throttling, startup/authentication errors, and supervisor cancellation. A
watchdog-truncated run is not proof of native completion behavior.

The fixtures contain sanitized output from four explicitly bounded live calls:

| Directory | Date | Offline classifier expectation |
|---|---|---|
| `fixtures/allowed-warning/` | 2026-10-06 | Available harness; a normally completed request with warning/reset fields must not pause it |
| `fixtures/authentication-failed/` | 2026-10-13 | Ordinary failure; expired OAuth is not a temporary limit |
| `fixtures/weekly-rejected/` | 2026-10-13 | Observed temporary weekly limit; top-level reset agrees with the rejected weekly window and native text |
| `fixtures/weekly-resumed/` | 2026-10-14 | Available harness; the same limited session resumed successfully after reset |

Each `run.json` documents field omissions; `events.jsonl` retains all four native
records in order; `stderr.log` is empty. These are offline classifier inputs only:
normal CI and `make test` never execute this script, call a paid harness, or
attempt quota exhaustion. No fixture carries a Mergeyard phase schema or
`structured_output`, so the warning is not a successful engineering-phase result
fixture. Do not invent positive fixtures for unobserved variants or infer
limited-session resumability from a session UUID alone.

`fixtures/weekly-resumed/` preserves the single post-reset resume on 2026-10-14.
It used the same session UUID and original capture workspace, with `--resume`
replacing `--session-id` and all safety bounds/settings reapplied. The successful
completion confirms resumability for this limited session. See the exact argv
in `run.json` and the report's observed facts. A reproduction requires that
persisted session and its original workspace; a fresh session is not a resume
check. The original capture supervisor was reused for the bounded request.

Reuse [Claude M1 continuity evidence](../harness-spikes.md#6-issue-9-live-m1-adapter-verification-2026-10-04)
for resume/interactive handback; this probe does not repeat those checks.
