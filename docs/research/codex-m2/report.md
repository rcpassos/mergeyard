# Issue #37: live Codex M2 evidence

Date: 2026-10-05. Scope: [issue #37](https://github.com/rcpassos/mergeyard/issues/37)
and its [M2 parent](https://github.com/rcpassos/mergeyard/issues/36). This is an
evidence gate for future adapter work, with no application implementation.
Claude M1 checks, quota exhaustion, usage-limit waiting, and interactive handback
were excluded.

## Environment and reproduction

Installed binary: `/opt/homebrew/bin/codex`, **`codex-cli 0.156.1`**. Version and
both exec help commands exited 0. `codex login status` exited 0 and reported
`Logged in using ChatGPT`. Host: macOS, zsh, linked Git worktrees, Go 1.24 module.
Model requests ran outside the outer Codex desktop execution sandbox so the
fixture CLI could authenticate and reach the service. The child CLI still used
its own explicitly selected workspace-write or read-only sandbox.

The suite made 16 explicitly selected CLI invocations, each with a watchdog;
there was one explicit retry after a rejected model. A 120-second bound applied
to each invocation except the 25-second unauthenticated control. No watchdog
expired. Codex itself retried transport authentication failures. There was no
dollar budget flag; these were short tasks using `gpt-6-luna` at low/medium effort,
apart from the rejected `gpt-6.1-sol` request. Recorded usage is per-turn CLI
telemetry, not a bill. Normal repository tests never launched a paid harness.

See [README.md](README.md) for the complete manual recipe and cost controls,
[probe.py](probe.py) for capture/checking, and [evidence.json](evidence.json) for
exact command arrays, exit codes, selected unmodified event/tool outputs, final
results, effective turn contexts, and skill injection evidence. Raw transcripts
and stderr remain in a private temporary fixture and are not durable evidence.
The selected repository evidence is the durable record. Authentication files,
account identifiers, and unrelated session contents are excluded.

Fixture root (the `ROOT` shorthand below):
`/private/var/folders/9d/rccfzf_d2wq6g69c0hrnvwsr0000gn/T/mergeyard-codex-m2-68jjyi04`.
`ROOT/base` owns Git metadata; `ROOT/worktree` and `ROOT/other` are linked
worktrees. Inputs, schemas, evidence, and private `CODEX_HOME` are outside the
worktrees. Only fixture auth and model metadata were staged, never user config.
Fixture auth was removed after every invocation. Personal skill discovery was
not disabled, but no personal skill invocation was observed.

All model commands used this prefix, before the optional `resume ID`:

```sh
codex exec --ignore-user-config --ignore-rules --json \
  -C "$ROOT/worktree" -s workspace-write -m gpt-6-luna \
  -c model_reasoning_effort=low \
  -c sandbox_workspace_write.network_access=false \
  -c features.plugins=false -c 'web_search="disabled"' \
  --output-schema "$SCHEMA" -o "$CASE/last-message.json"
```

The trusted final positional prompt contains `$m2-amber` / `$m2-blue` where
selected, then `Read ROOT/inputs/PHASE.md and follow it. Do not commit.` Input
markers and skill markers are absent from that prompt. Stdin was `/dev/null`.
No `--full-auto`, `--last`, `--ephemeral`, or approval bypass was used. `-C` and
`-s` precede `resume`, matching installed help. Effective approval policy was
`never` in all persisted turn contexts. A schema-valid `blocked` or `failed`
result means the model completed its report, not that the workflow succeeded.

## Questions and classifications

**Verified** means supported by the captured live outputs; **contradicted** means
an observed counterexample to a proposed behavior; **blocked** means the probe
did not establish the behavior. These are version/account/environment-specific.

| Question | Classification | Evidence and limit |
|---|---|---|
| Early identity, exact-ID resume, continuity | Verified | Implement emitted `thread.started` before process exit; fix reused that UUID and recovered the seed marker after its input file was deleted. |
| External phase input read | Verified | Implement, review, fix, and fresh-session commands read the external files successfully. |
| Strict implement/review/fix schemas | Verified | All three contracts produced exact required fields; nested findings and fix responses validated. |
| Schema output on resumed sessions | Verified | Fix and later review completed with their supplied phase schema. |
| Native completion; reject failures/incomplete output | Verified | Completed turns had exit 0, `turn.completed`, and valid last-message JSON. Failures and signalled runs failed the offline checker. |
| Effective model selection on resume | Verified | The rejected-model turn context records `gpt-6.1-sol`; the provider explicitly rejected that requested model. The retry records `gpt-6-luna`. Overrides were not silently ignored. |
| Successful conversation continuation across different models | Blocked | `gpt-6.1-sol` was rejected by this CLI/account. No second accepted model was tried; model-cache presence alone proves no entitlement. |
| Effort, sandbox, cwd, network settings on resume | Verified | Fix changed low → medium effort and network false → true. Settings changed back to low, selected read-only, moved to `other`, and denied writes/network. |
| Unknown-session signals versus auth/config errors | Verified | Unknown UUID produced a specific missing-rollout stderr diagnostic, exit 1, no JSONL identity. Auth returned 401; invalid effort returned 400 with `turn.failed`. |
| Fresh session gets full phase input | Verified | Separate fresh invocation read the same input and returned `full_input_teal_826`, with a new UUID and native completion. |
| Explicit configured skill execution | Verified | Injected `<skill>` body in rollout plus body-only marker in structured summary; invocation prompt contains only skill names. |
| Multiple skills, including resume | Verified | Review and resumed review both injected amber and blue bodies and returned both markers. |
| Linked-worktree editing and Git inspection | Verified | Implement/fix file-change events and actual `git status` / `git diff` outputs; base Git metadata remained outside worktree. No agent commit probe was required or run. |
| Representative test/build caches inside worktree | Verified | Actual `GOCACHE="$PWD/.cache/go-build" go test ./...` and `go build ./...` exited 0 in implement, fix, and cache probes. |
| Default external Go test cache writable | Contradicted | Default `go test ./...` exited 1 with `operation not permitted` opening the user cache's `trim.txt`. |
| Default external Go build cache behavior | Blocked | The model skipped the requested default-cache build. Its summary claimed failure, but there is no executed-command evidence for that claim. |
| Configured network and permission behavior | Verified | Read-only shell write exited 1; network-off curl exited 6. Workspace-write/network-on curl exited 0, HTTP/2 200. Tool output, not model prose, establishes this. |
| Ordinary SIGINT / SIGTERM and retained identity | Verified | Both interrupted turns lacked native completion and valid output; subsequent exact-ID resumes recovered the interrupted marker. See signal evidence below. |
| Strict output implies semantically correct review | Contradicted | Resumed review produced `changes_required` with `findings: []` after observing Value() == 3. Native schema enforcement does not enforce status/finding relationships. |

## Identity, contracts, and configured skills

Implementer: `01a10c5b-38a9-7330-b833-04e365cb5f37`.
Reviewer: `01a10c5c-8a43-7c52-ba89-5a6d6ec1e22c`.
The supervisor flushed `identity.json` upon each `thread.started`, with
`identity_before_exit: true`. Roles used distinct conversations. Fix reused the
implementer UUID; later review reused the reviewer UUID. Actual implement test,
build, Git inspection, and file edits succeeded with exit 0. The first review
read its input but did not inspect Git; the later resumed review did inspect
`git diff` and `nl -ba value.go`. Neither schema success nor the first summary
proves that omitted command ran.

Implement result (exit 0, followed by `turn.completed`) included
`schema_version: 1`, `status: "success"`, and a summary containing
`skill_amber_257` and `input_amber_492`. Review returned a nested blocking finding
`R1-F1`, file `value.go`, line 3, with `status: "changes_required"`.
The resumed fix changed implementation and test to 3 and returned:

```json
{
  "schema_version": 1,
  "status": "success",
  "summary": "R1-F1 fixed: `Value()` and its expected test result are 3. `go test ./...` and `go build ./...` passed using the requested GOCACHE. Changes remain uncommitted. Markers: continuity_cobalt_731, skill_amber_257.",
  "responses": [{"finding_id": "R1-F1", "resolution": "fixed", "note": "Updated the implementation and test expectation to 3."}]
}
```

The seed input was removed before fix, and fix commands read only its own phase
input and worktree files, not earlier evidence. This demonstrates retained
conversation continuity. No resumed prompt restated `continuity_cobalt_731`.
Both skill fixtures set `policy.allow_implicit_invocation: false` in
`agents/openai.yaml`. Rollouts contain user `response_item` entries with
`<skill><name>m2-amber</name>...` and, for both reviews, `m2-blue`. Their bodies
contain the markers absent from the phase input. The resulting summaries contain
`skill_amber_257` and `skill_blue_864`. The same injections occurred again on
resume; schema validity alone was not used to infer skill execution.

## Resume settings and missing-session controls

Fix first requested `gpt-6.1-sol`, medium effort, workspace-write, network true.
Its turn context records those exact values. Exit 1, `error` and `turn.failed`:
`The 'gpt-6.1-sol' model is not supported when using Codex with a ChatGPT account.`
CLI also warned that metadata for this model was not found. An explicit retry
on the same UUID requested `gpt-6-luna` at medium effort and completed.

The settings probe resumed that UUID with `-C ROOT/other -s read-only`, low
effort, and network false. Turn context and actual `pwd` both show `ROOT/other`.
The shell returned `operation not permitted: denied.txt`, `WRITE_EXIT_CODE=1`,
and `curl: (6) Could not resolve host: example.com`, `CURL_EXIT_CODE=6`.
The denied file was not created. Its result still recovered the original marker.

Unknown-ID invocation: explicit
`resume 00000000-0000-4000-8000-000000000037`, using the complete external
`unknown.md` input. Exit **1**, no stdout events, stderr:

```text
Error: thread/resume: thread/resume failed: no rollout found for thread id 00000000-0000-4000-8000-000000000037 (code -32600)
```

Recovery was a separate new `exec`, with the same schema, working directory,
model/effort, sandbox, and complete input; not a retry of the missing UUID.
It produced new UUID `01a10c61-253c-7173-a0d7-d82901ccda96`, exit 0, valid
implement result containing `full_input_teal_826`, and `turn.completed`.

Authentication control used an empty separate `CODEX_HOME` with key environment
variables removed. It emitted `thread.started`, 401 authentication errors,
transport retries, and `turn.failed`; exit 1. Invalid effort likewise emitted
identity, `error` and `turn.failed`, exit 1, but its error was a 400
`[ReasoningEffortParam] ... Invalid value: 'invalid-probe-value'`.
Non-strict schema returned exit 1 and `invalid_json_schema`, explaining that
`required` must include `optional_marker`. All three lacked a valid result.
Their identity events alone must not trigger missing-session recovery.

## Signals and completion

The supervisor sends signals to the CLI process group immediately after the
JSONL `item.started` command containing `sleep 30`. Identity was already flushed
before signalling. The interrupted phase input is removed before restart;
restart input asks for the earlier marker without naming it.

SIGINT exited **1**, with stdout ending at the in-progress `sleep 30` command.
There was no `turn.completed`, no `turn.failed`, and no valid last-message JSON.
Stderr included `exec_command failed: UnknownProcessId`. The captured identity
was `01a10c63-7715-7160-b39b-9bf62d36743b`.

SIGINT restart reused that UUID, exited **0**, emitted `turn.completed`, and
returned `interrupted_violet_619` in a valid implement result. Its only shell
command read `restart.md`; it did not reread the deleted interruption input.

SIGTERM retained UUID `01a10c66-f486-7d72-afed-54b1f2dc9f18` and exited **-15**
in Python's subprocess convention (signal 15; shell convention would be 143).
Its stream likewise ended at the unfinished `sleep 30`, without a completion
or failure event and without valid final output. Its exact-ID restart exited
**0**, returned `interrupted_violet_619`, and emitted `turn.completed`.
All signal, exit, and result values are recorded in `evidence.json`.
No interactive session or handback was tested. Offline completion controls also
rejected valid JSON paired with missing completion, nonzero exit, `turn.failed`,
or supervisor interruption, and rejected extra result fields. These controls
used generated artifacts without calling a model.

## Required adapter adjustments

1. Replace PRD §11's “any failed resume before `thread.started`” fallback with
   specific missing-rollout evidence, scoped to the requested ID. Auth, config,
   network, permission, and unsupported-model failures must remain failures.
   Preserve the complete phase input on one explicitly bounded fresh recovery.
2. Persist the early UUID and pass every setting/schema/skill on every invocation.
   Cross-model continuation is blocked for the rejected model; model metadata is
   insufficient entitlement evidence. Expose its provider error instead of
   silently changing models or discarding conversation state.
3. Keep workspace-write for reviewers that run tests. Provide a writable cache
   path (verified: in-worktree GOCACHE), or explicitly authorize additional cache
   directories and verify that configuration. Default host cache access cannot
   be assumed; other runtimes and cache locations remain untested.
4. Require process exit 0, native completion, and validated final output together.
   Interruption can truncate stdout without a failure event. Keep the flushed
   identity for ordinary restart and wait for the owned process to exit before
   resuming it. Never accept a stale last-message file from a previous attempt.
5. Validate result semantics in Mergeyard: status/finding consistency and fix
   coverage are not guaranteed by strict JSON Schema. Inspect actual tool output
   when claiming edits, tests, permissions, or skill execution, rather than
   trusting summaries. Fixture schemas verify shape, not task correctness.

These observations refine the historical recommendations in
[harness-spikes.md](../harness-spikes.md); they do not claim the dependent M2
adapter or workflow has been implemented.

## Repository validation

`go test ./internal/harness`, `go build ./...`, `go vet ./...`, and the full
`go test ./...` passed, using an explicitly writable temporary GOCACHE. The
initial targeted test attempt inside the outer sandbox could not launch its
tmux-backed fixture wrapper; the targeted suite and final full suite passed
outside that sandbox. Python syntax, schema JSON parsing, and offline positive/
negative completion controls passed. No paid model was invoked by these checks.
No new automated test seam or production behavior was introduced.

The required Standards and Spec reviews both identified the same watchdog gap:
a descendant holding stdout open could outlive the CLI leader and prevent
capture from ending. The fixture supervisor now signals the group independently
of leader liveness, stops capture at the hard deadline, avoids duplicate group
kills, and preserves auth cleanup and metadata even on shutdown errors. Local
fake-CLI controls verified normal success and the surviving-descendant case:
with a 10-second timeout and five-second grace, the latter stopped at 15.07
seconds, saved metadata, rejected completion, and left no surviving descendant.
No model or network call was made by those controls. Both review axes have no
remaining actionable findings against the user-confirmed baseline `422a798`.

Documentation cross-check: installed help plus Context7 `/openai/codex` source
references for exec flags, thread events, and skill injection, and the official
[non-interactive mode documentation](https://learn.chatgpt.com/docs/non-interactive-mode).
The live command evidence above controls all version-specific conclusions.
