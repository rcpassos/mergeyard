# Codex M3 usage-limit source expectations

Checked 2026-10-06 for [issue #62](https://github.com/rcpassos/mergeyard/issues/62).
These are documentation and source findings, not live verification. No model
request or personal-session scan was performed for this note.

Context7 resolved `/openai/codex` (the official repository), then queried the
non-interactive usage-limit event contract. Its available versions did not
include 0.156.1, and its snippets referenced `main`; the version-specific claims
below were checked against `rust-v0.156.1`, which resolves to commit
[`b412ff32c417f855c2b2d1581b77058eed87c84b`](https://github.com/openai/codex/commit/b412ff32c417f855c2b2d1581b77058eed87c84b).

## Public JSONL contract

The current official documentation says `codex exec --json` writes a JSONL
event stream to stdout, including `thread.started`, `turn.started`,
`turn.completed`, `turn.failed`, and `error`. It documents explicit session-ID
resume commands. This establishes the supported automation surface, but does
not establish a usage-limit reset epoch or first-request failure ordering.
[Non-interactive mode](https://learn.chatgpt.com/docs/non-interactive-mode).

In 0.156.1, `ThreadErrorEvent` contains only `message`; both `error` and
`turn.failed.error` use that type. Neither exposes `codex_error_info` nor a
structured reset time. `thread.started.thread_id` is the resume identifier.
[Event definitions](https://github.com/openai/codex/blob/b412ff32c417f855c2b2d1581b77058eed87c84b/codex-rs/exec/src/exec_events.rs#L6-L87).

An error notification emits `error`; a failed turn completion emits
`turn.failed`, carrying the turn's error message, the last critical error, or a
generic fallback. Additional details can be appended to the message. Thus an
`error` followed by `turn.failed` is the expected mapping when both upstream
notifications arrive, rather than an unconditional guarantee about every
failure. Account-rate-limit notifications are not emitted by this processor.
[JSONL processor](https://github.com/openai/codex/blob/b412ff32c417f855c2b2d1581b77058eed87c84b/codex-rs/exec/src/event_processor_with_jsonl_output.rs#L426-L577).

The executable records a non-retrying error or failed/interrupted completion
for its primary turn and exits 1. That exit code also covers unrelated errors,
so it cannot identify usage limits by itself.
[Executable lifecycle](https://github.com/openai/codex/blob/b412ff32c417f855c2b2d1581b77058eed87c84b/codex-rs/exec/src/lib.rs#L1202-L1264).

## Reset and rollout expectations

`UsageLimitReachedError` renders plan-specific recovery text with a stable
usage-limit phrase. Model-specific limits have a separate variant. Missing
reset times produce a retry-later suffix. Workspace-credit exhaustion and
workspace spend caps have distinct messages requiring action. The formatter
converts UTC reset times to machine-local time: time only on the same local
day, otherwise an English month, ordinal day, year, and time. Message wording
is version-sensitive and minute precision loses seconds.
[Error formatting](https://github.com/openai/codex/blob/b412ff32c417f855c2b2d1581b77058eed87c84b/codex-rs/protocol/src/error.rs#L618-L752).

The native protocol distinguishes `UsageLimitExceeded` from
`RateLimitExceeded`. `TurnCompleteEvent.error` can retain the structured error.
`TokenCountEvent.rate_limits` is optional; its snapshot has optional primary
and secondary windows. A window's `resets_at` is explicitly Unix epoch
seconds, and `window_minutes` is its duration in minutes. Null means absent
information, not a zero reset time.
[Native protocol](https://github.com/openai/codex/blob/b412ff32c417f855c2b2d1581b77058eed87c84b/codex-rs/protocol/src/protocol.rs#L1721-L2227).

On a sampling `UsageLimitReached` error, the code updates rate limits before
returning the error **if the error carries a snapshot**. This supports the
expectation that a snapshot can be available on the first rejected request;
it does not prove that the backend always supplies one.
[Sampling error handling](https://github.com/openai/codex/blob/b412ff32c417f855c2b2d1581b77058eed87c84b/codex-rs/core/src/session/turn.rs#L1599-L1609).

Rollout persistence includes token-count and turn-complete events, while
standalone error events are transient. The rollout recorder writes queued
items asynchronously and has explicit flush/shutdown barriers with I/O error
paths. This supports inspecting a thread's completed rollout for structured
limits; it does not establish durable write timing for an interrupted process.
The rollout is an internal source representation, so treat extraction as
version-bound evidence rather than a documented stable integration contract.
[Persistence policy](https://github.com/openai/codex/blob/b412ff32c417f855c2b2d1581b77058eed87c84b/codex-rs/rollout/src/policy.rs#L86-L144),
[Recorder](https://github.com/openai/codex/blob/b412ff32c417f855c2b2d1581b77058eed87c84b/codex-rs/rollout/src/recorder.rs#L981-L1104).

## Still requires live evidence

Confirm the installed version's first rejected request: exact stdout event
sequence, stderr, exit code, matching thread rollout, whether its token-count
snapshot precedes the failed task completion, and which exhausted window
matches the displayed reset. Record a same-thread resume after that reset to
establish recovery. Source inspection or a fake CLI fixture cannot verify
backend rejection, reset availability, or post-reset recovery.
