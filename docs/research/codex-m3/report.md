# Issue #63: Codex interactive takeover and handback

Observed on 2026-10-06 with `codex-cli 0.156.1`, macOS, and `gpt-6-luna`.
This completes the bounded continuity probe requested by
[issue #63](https://github.com/rcpassos/mergeyard/issues/63), under the
[M3 specification](https://github.com/rcpassos/mergeyard/issues/60).
The [Claude Q10 evidence](../harness-spikes.md#q10--interactive-takeover-and-headless-handback)
remains the baseline; it was not repeated or changed.

## Observed result

All stages used session `01a11325-8600-77e2-a2c1-382b1f3bf4fc` in one
linked worktree of a private, disposable repository. No picker, `--last`,
branch, clear, or concurrent resume was used.

| Stage | Evidence | Result |
|---|---|---|
| Headless interruption | `thread.started`, `sleep 30` command-start, supervisor SIGINT record | Identity captured before exit; exit 1 after 9.95 seconds; no native completion. |
| Interactive takeover | Exact-ID `codex resume`, recorded terminal, native rollout | Same interrupted history displayed. Two short manual model turns; clean `/exit`, exit 0 after 99.35 seconds. |
| Normal interactive permissions | Effective `on-request`/`workspace-write` context, approval menu and command result | The harmless write outside the worktree waited for “Yes, proceed”; one-time `y` approval allowed it, exit 0. |
| Headless handback | Exact-ID `codex exec … resume`, JSONL, native implement schema | Exit 0 after 10.51 seconds, `turn.completed`, no error/failed event, valid result with the manual marker. |

The final native result was:

```json
{"schema_version":1,"status":"success","summary":"skill_amber_257; manual_coral_863; interrupted_violet_619"}
```

`manual_coral_863` was introduced only through interactive terminal input.
The final prompt and phase input did not repeat it. The seed input was deleted
before takeover, and the manual marker was absent from all worktree files.
The final JSONL records exactly one command: reading the marker-free handback
input. There was no read of sessions, terminal logs, or evidence files.
Thus the final turn recovered the manual context from the same conversation.
The skill marker also shows the explicitly reapplied `m2-amber` skill was followed.

## Process ordering and settings

The existing [M2 supervisor](../codex-m2/probe.py) sent SIGINT upon the live
`sleep 30` command-start event, waited for the CLI exit, and removed staged
authentication before returning. Only after that supervisor completed did the
interactive wrapper start. The rollout records the interrupted tool as
`aborted by user after 0.2s`; the terminal displayed “Conversation interrupted.”
This is interruption evidence, not successful phase completion.

The [interactive wrapper](interactive.py) ran the actual TUI through macOS
`/usr/bin/script`. It waited for `/exit`, reaped the CLI, stopped any remaining
owned process-group descendants, saved its exit record, and removed staged
authentication before the final headless invocation. Raw terminal recording
stayed outside the worktree. `TERM=dumb` produced a warning; the operator
answered `y` to continue. No trust dialog was observed in this fixture.

[Versioned evidence](evidence.json) preserves exact argv with private paths
replaced by `ROOT` and `REPO`, native JSONL, selected rollout records, and
effective `turn_context` values. The relevant settings were:

| Setting | Interrupted headless | Interactive | Final headless |
|---|---|---|---|
| Model | `gpt-6-luna` | `gpt-6-luna` | `gpt-6-luna` |
| Effort | `low` | `medium` | `low` |
| Approval policy / reviewer | `never` / `user` | `on-request` / `user` | `never` / `user` |
| Sandbox | `workspace-write` | `workspace-write` | `workspace-write` |
| Tool network access | false | false | true |
| Temporary-directory write exclusions | false | true | false |
| Completion contract | Native implement schema | Prose | Native implement schema |

The interactive exclusions made the sibling `ROOT/approval.txt` a protected
write target even though the entire fixture was under a temporary directory.
Normal approval was exercised without granting full access or bypassing the
sandbox. The final turn explicitly reapplied the headless settings, output
schema, output path, and skill; it did not inherit the manual permission policy.
The unchanged worktree and `permission-approved\n` sibling file were inspected
afterward. Staged authentication was absent after each invocation.

## Permission observation caveat

The first interactive turn acknowledged retaining the markers but claimed that
its policy prohibited `require_escalated`, without attempting a command. Its
native context actually showed `on-request`. The operator inspected
`/permissions`, which displayed “Ask for approval (current),” and requested an
actual tool attempt. The second interactive turn produced the normal
[approval menu](approval-terminal.txt); the approved command then completed.
Both manual turns, including the initial refusal, are retained in the evidence.
The successful tool attempt establishes observed permission handling; the
model's earlier claim does not establish an enforcement failure.

## Bounds and remaining release dependencies

This probe made four model turns in three CLI invocations: one interrupted
headless turn, two interactive turns, and one final headless turn. Headless
watchdogs were 120 seconds; the interactive watchdog was 180 seconds, followed
by bounded shutdown. No timeout or automatic model retry occurred. The fixture
has a 16-invocation hard cap shared with the M2 runner. Codex exposed no dollar
budget flag; these calls consumed the authenticated account's quota.

The same-session continuity contract and explicitly selected normal interactive
approval flow are verified for this version and fixture. This does not prove
Mergeyard's future takeover/handback scheduler, reviewer restoration, dirty
manual publication, blocked-harness recovery, or crash/concurrency behavior.
Those remain release dependencies of their M3 implementation tickets. Live
usage-limit/credit behavior remains a separate release dependency; this probe
neither exhausted quota nor treated account metadata as a limit failure.

A bare `codex resume -C WORKTREE SESSION_ID` using an operator's personal
configuration, without this probe's explicit permission flags, was not tested.
Before releasing a takeover invocation that relies on that default, verify its
effective normal interactive permissions rather than assuming that an earlier
headless policy will be discarded. Different models, versions, inherited user
configuration, and cleared/branched identities are not established here.
Adopting a different identity remains outside scope.

These live tools are excluded from application execution, `make test`, and CI.
The [reproduction recipe](README.md) is explicitly opt-in. Sanitization removes
credentials, personal paths, quota/account metadata, personal skill catalogs,
and reasoning; selected observed records remain versioned. The existing M2
completion checker provides an offline check without a model request.

## Documentation checked

The official [CLI reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli)
documents exact-ID interactive resume, `-C` directory selection, and
`workspace-write` with `on-request` for interactive use. The official
[non-interactive documentation](https://learn.chatgpt.com/docs/non-interactive-mode)
documents exact-ID headless resume. Context7 `/openai/codex` and local
`codex resume --help` / `codex exec resume --help` were also checked.
The live observations above establish continuity and effective settings;
documentation alone is not used as a claimed probe pass.
