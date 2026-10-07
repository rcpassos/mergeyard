# Manual Claude M3 usage-limit capture

Evidence for [issue #61](https://github.com/rcpassos/mergeyard/issues/61).
Read [report.md](report.md) before using the fixtures: the required headless
usage-limit failure has **not** been observed. The captured warning is a
successful control, not a positive failure fixture.

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

`fixtures/allowed-warning/` contains sanitized output from the one live call.
`run.json` documents field omissions; `events.jsonl` retains all four event
types in order; `stderr.log` is empty. These are offline classifier inputs only:
normal CI and `make test` never execute this script, call a paid harness, or
attempt quota exhaustion. Future classifier tests should treat this capture as
an available harness despite its warning/reset fields. It does not carry a
Mergeyard phase schema or `structured_output`, so it is not a successful
engineering-phase result fixture.

Reuse [Claude M1 continuity evidence](../harness-spikes.md#6-issue-9-live-m1-adapter-verification-2026-10-04)
for resume/interactive handback; this probe does not repeat those checks.
