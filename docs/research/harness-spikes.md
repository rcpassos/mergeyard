# Harness Spikes: Claude Code and Codex CLI

Research for PRD §37 (Pre-Implementation Spikes). Date: 2026-10-03.

## Versions examined

| Harness | Installed locally | Latest published | Sources used |
|---|---|---|---|
| Claude Code | `2.1.231 (Claude Code)` at `~/.local/bin/claude` | `2.1.288` (npm `@anthropic-ai/claude-code`, modified 2026-10-02) | `claude --help` (2.1.231); docs at code.claude.com (they describe features up to ~2.1.288); local transcripts written by 2.1.197–2.1.286 |
| Codex CLI | `codex-cli 0.156.1` at `/opt/homebrew/bin/codex` | `0.160.0` (npm `@openai/codex`); GitHub tag `rust-v0.159.3`; `main` @ `b741e48` (2026-10-03) | `codex … --help` (0.156.1); source at tag `rust-v0.156.1` (`b412ff3`), diffed against `main`; docs at developers.openai.com/codex, which now redirect (HTTP 308) to learn.chatgpt.com/docs/… |

Update (2026-10-03, after this research): Claude Code was updated locally to `2.1.288`, so features flagged as needing ≥2.1.277 (such as AGENTS.md support) are now available. The findings below were written against 2.1.231.

Note: the installed Claude Code is about 57 patch releases behind the docs. Docs features that need a newer version are flagged below. `claude --help` doesn't list every flag (the CLI reference says so), so a flag missing from 2.1.231's help doesn't prove it's missing from the binary.

Legend: **V-help** = local `--help` output · **V-docs** = official docs (URL) · **V-src** = Codex source at `rust-v0.156.1` · **V-local** = observed in local session files on this machine · **U** = unverified (reason given).

No command that calls a model was run.

---

## 1. Summary table

| Spike | Claude Code | Codex CLI |
|---|---|---|
| 1. Headless resume | `claude -p --resume <uuid> …` **V-help, V-docs**. You can pre-assign the ID with `--session-id <uuid>` **V-help**. The ID is also in `session_id` on every stream-json event and the final result **V-docs**. Lookup by ID works from any directory (≥2.1.223) **V-docs** | `codex exec [opts] resume <uuid> [PROMPT or -]` **V-help, V-src**. No way to pre-assign an ID **V-help**. Get the ID from the first JSONL event `{"type":"thread.started","thread_id":…}` **V-src, V-docs**, or from the rollout filename. Lookup by UUID skips the cwd filter **V-src** |
| 2. Structured output | `--output-format json\|stream-json` plus `--json-schema '<inline JSON>'`. Validated object lands in `structured_output`. Failure gives subtype `error_max_structured_output_retries` **V-help, V-docs** | `--output-schema <file>` (strict mode on by default) plus `-o <file>` (final message, which is a JSON string) and `--json` JSONL events **V-help, V-src** |
| 3. Usage limits | Text `You've hit your session limit · resets 7:30pm (Europe/Lisbon)`, with `error:"rate_limit"` and `apiErrorStatus:429` **V-local** (Desktop sessions, 2.1.197–2.1.286). In the SDK/stream: `rate_limit_event{status:"rejected", resetsAt:number}` and result `api_error_status` **V-docs**. Exit code and exact `-p` JSON are **U** | Exit code `1` on a failed turn **V-src**. JSONL `turn.failed.error.message` = `You’ve hit your usage limit. … try again at 6:23 PM.` **V-src, V-local**. The rollout file has `codex_error_info:"usage_limit_exceeded"` and `rate_limits.primary.resets_at` (epoch seconds) **V-local** |
| 4. Skills | Dirs: `~/.claude/skills/<name>/SKILL.md`, `<repo>/.claude/skills/…`, nested, plugins, `--add-dir`. Invoke in `-p` by putting `/skill-name args` in the prompt, or let the model invoke it automatically **V-docs** | Dirs: `<cwd>/.agents/skills`, `<repo-root>/.agents/skills`, `~/.agents/skills`, `/etc/codex/skills` **V-docs**. Invoke with a `$skill-name` mention anywhere in the text input (parsed in core, so it works in `exec`) **V-src**, or implicitly **V-docs** |
| 5. Model / effort | `--model <alias\|id>`, `--effort low\|medium\|high\|xhigh\|max` **V-help**. Env `ANTHROPIC_MODEL`, `CLAUDE_CODE_EFFORT_LEVEL` **V-docs**. An unsupported effort falls back to the highest supported level below it **V-docs** | `-m <model>` **V-help**. `-c model_reasoning_effort=<level>`, levels depend on the model (`low…max`, some add `ultra`) **V-docs, V-local**. No effort flag |
| 6. Interactive resume | `claude --resume <uuid>` (from the worktree). `-p` sessions are left out of the picker and out of `--continue`, but resume fine by ID **V-docs** | `codex resume <uuid>` (or `-C <worktree>`). Exec sessions are hidden from the picker and `--last` unless you pass `--include-non-interactive`. Explicit UUID lookup reads the thread directly **V-help, V-src** |
| Unattended perms | `--permission-mode bypassPermissions` (= `--dangerously-skip-permissions`), `acceptEdits`+`--allowedTools`, `auto`, `dontAsk` **V-help, V-docs**. A `-p` run defaults to `default` (prompts get denied) **V-docs** | `exec` forces `approval_policy=never` **V-src**. The default sandbox is `read-only`, so you need `-s workspace-write` **V-docs**. Network is off in workspace-write unless `sandbox_workspace_write.network_access=true` **V-docs, V-src**. `--full-auto` is deprecated and missing from 0.156.1 help **V-docs, V-help** |
| Instruction files | CLAUDE.md always. AGENTS.md only when no CLAUDE.md/CLAUDE.local.md exists (needs ≥2.1.277, so **not** the installed 2.1.231) **V-docs** | AGENTS.md / AGENTS.override.md only. CLAUDE.md only through `project_doc_fallback_filenames` **V-docs** |
| Config dir env | `CLAUDE_CONFIG_DIR` (default `~/.claude`) **V-docs** | `CODEX_HOME` (default `~/.codex`) **V-help** |

---

## 2. Claude Code (detailed)

### 2.1 Headless resume and session IDs

- **New session with a known ID:** `claude -p --session-id <uuid> …`. The help text says the value "must be a valid UUID". **V-help (2.1.231)**, https://code.claude.com/docs/en/cli-reference
- **Resume with a new prompt:** `claude -p --resume <session-id> "prompt"`. The docs example captures the ID with `--output-format json | jq -r '.session_id'`. **V-docs** https://code.claude.com/docs/en/headless#continue-conversations
- **Lookup scope:** "Claude Code finds the session by its ID in any project on this machine. Before v2.1.223, Claude Code looked for the ID only in the current project directory and its git worktrees." **V-docs** (headless, sessions). 2.1.231 is past that change. Mergeyard always runs from the worktree anyway.
- **`--continue` is unsafe for Mergeyard.** It picks "the most recent conversation" in the cwd. Always use explicit IDs. **V-docs**
- **Storage:** `~/.claude/projects/<cwd with non-alphanumerics replaced by '->/<session-id>.jsonl`. The format is internal and "changes between versions". Retention is 30 days by default (`cleanupPeriodDays`). Can be relocated with `CLAUDE_CONFIG_DIR` (+ `CLAUDE_CODE_PROJECT_DIR_NAME` ≥2.1.234). **V-docs** https://code.claude.com/docs/en/sessions#where-transcripts-are-stored; layout **V-local** (`~/.claude/projects/-Users-…-mergeyard/<uuid>.jsonl`).
  - **Risk:** a run that waits longer than `cleanupPeriodDays` (30 days by default) could lose its session to the retention sweep. The resume-failure fallback in PRD §11 covers this.
- **Resume failure message:** `No conversation found with session ID: <session-id>`. **V-docs** https://code.claude.com/docs/en/errors
- **Flags that must not be used:** `--no-session-persistence` (the session can't be resumed). `--bare` turns off OAuth/keychain: "Anthropic auth is strictly ANTHROPIC_API_KEY or apiKeyHelper". It also skips CLAUDE.md and skills. That breaks subscription use. **V-help**
- **What a resumed session keeps:** history and model (unless `--model` is passed). It does **not** keep `--mcp-config`, `--settings`, `--plugin-dir`, `--add-dir`, or `--fallback-model`; pass them again every time. `-p --resume` starts in the permission mode a new `-p` run would use. So pass permission flags on every invocation. **V-docs** https://code.claude.com/docs/en/sessions#what-a-resumed-session-restores
- **System prompt:** `--append-system-prompt` text is recorded on the conversation's first request and reused on resume until compaction. Changing it later has no effect. Role instructions placed there are fixed when the session is created. **V-docs** (cli-reference, "System prompt flags in resumed conversations")
- **Stopping:** SIGTERM gives exit `143` and leaves "the turn that was in progress unfinished". SIGINT ends the turn. `CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1` makes resume continue the interrupted turn. **V-docs** https://code.claude.com/docs/en/headless#stop-a-run-with-sigterm

### 2.2 Structured output

- `--output-format text|json|stream-json` (print mode only). `--json-schema <schema>` takes an **inline** JSON Schema string. **V-help**
- With `--json-schema`, "the structured output [is] in the `structured_output` field". An invalid schema exits with `Error: --json-schema is not a valid JSON Schema` (≥2.1.205). `format` is treated as an annotation only. **V-docs** https://code.claude.com/docs/en/headless#get-structured-output
- Validation works by re-prompting on mismatch. If retries run out, the result has subtype `error_max_structured_output_retries`. Supports `enum`, `const`, `required`, nested objects, and `$ref`. **V-docs** https://code.claude.com/docs/en/agent-sdk/structured-outputs
- **Final result shape** (`SDKResultMessage`; `--output-format json` prints the same object). **V-docs** https://code.claude.com/docs/en/agent-sdk/typescript (raw `.md`):
  - Success arm: `type:"result"`, `subtype:"success"`, `session_id`, `is_error`, `api_error_status?`, `num_turns`, `result` (string), `stop_reason`, `duration_ms`, `total_cost_usd`, `usage`, `modelUsage`, `permission_denials[]`, `structured_output?`, `terminal_reason?`
  - Error arm: `subtype` ∈ `error_max_turns | error_during_execution | error_max_budget_usd | error_max_structured_output_retries`, plus `errors: string[]`, `startup_failure_reason?`
  - `terminal_reason` ∈ `completed, max_turns, …, api_error, budget_exhausted, structured_output_retry_exhausted, …`
  - The `json`-vs-SDK field parity on 2.1.231 is **U** (the docs track the current version).
- **stream-json:** first `system/init` event (fields: `session_id`, `model`, `permissionMode`, `skills[]`, `slash_commands[]`, `plugins`, `effort`, `claude_code_version`, `cwd`, `tools`), then `assistant`/`user` messages, then one `result` line. **V-docs**. Docs examples always pair `stream-json` with `--verbose`. Whether 2.1.231 *requires* `--verbose` is **U**; pass it anyway.
- Cost/usage on a resumed run reports "the conversation's whole total, earlier runs' spend included". **V-docs**

### 2.3 Usage limits

- **Message strings (docs):** `You've hit your session limit · resets 3:45pm`, `You've hit your weekly limit · resets Mon 12:00am`, `You've hit your Opus limit · resets 3:45pm`, `You've hit your Sonnet limit · resets 3:45pm`. Spend/credit variants: `You've hit your monthly spend limit · …`, `… individual spend limit …`, `… org's monthly spend limit …`. These have no reset time, or end with `· your session limit resets 3:45pm`. **V-docs** https://code.claude.com/docs/en/errors
- **Observed on this machine (V-local, Claude Desktop transcripts, versions 2.1.197–2.1.286):** an assistant record with `"model":"<synthetic>"`, `"error":"rate_limit"`, `"isApiErrorMessage":true`, `"apiErrorStatus":429`, and text such as:
  - `You've hit your session limit · resets 7:30pm (Europe/Lisbon)`
  - `You've hit your session limit · resets 5pm (Europe/Lisbon)` (no minutes)
  - `You've hit your weekly limit · resets 11am (Europe/Lisbon)` (no weekday, unlike the docs example)
  - `You've hit your individual spend limit · run /usage-credits to raise it, or visit claude.ai/admin-settings/usage`
- **Reset-time format:** local wall-clock `h[:mm]am|pm`, optionally prefixed by a weekday (`Mon`), optionally followed by `(IANA tz)`. No date. Read it as the next time that wall-clock moment occurs in the given time zone.
- **Machine-readable path:** stream-json emits `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"|"allowed_warning"|"rejected","resetsAt?":number,"utilization?":number,"errorCode?":"credits_required",…},"session_id":…}`. Assistant messages carry `error:"rate_limit"` (429 against quota), as opposed to `overloaded` (529). The result carries `api_error_status`. **V-docs** (agent-sdk/typescript). The unit of `resetsAt` (seconds or ms) is **U**.
- **Exit code:** "exits with code 0 on success and a non-zero code when the run fails … When a failure happens inside the run … prints the failure as the result on stdout". **V-docs**. The exact `-p` result for a usage limit (`is_error:true`, `subtype`, `result` text, `api_error_status:429`, exit code 1) is **U**. Every observed record came from interactive/Desktop sessions.
- Auto-wait-and-continue after a reset applies only to interactive sessions (≥2.1.234). **V-docs**

### 2.4 Skills

- **Locations:** personal `~/.claude/skills/<name>/SKILL.md`; project `.claude/skills/<name>/SKILL.md`; nested `<subdir>/.claude/skills/…`; plugin `<plugin>/skills/<name>/SKILL.md` (invoked as `/plugin:skill`); enterprise managed dir; `--add-dir` dirs. Legacy `.claude/commands/<name>.md` still works. **V-docs** https://code.claude.com/docs/en/skills. Personal dir **V-local**.
- **Format:** a directory with `SKILL.md`, YAML frontmatter, all fields optional: `name`, `description`, `when_to_use`, `disable-model-invocation`, `user-invocable`, `allowed-tools`, `model`, `effort`, `context: fork`, `agent`, `arguments`, `paths`, …; the body supports `$ARGUMENTS`/`$0`/`$name` and `${CLAUDE_SESSION_ID}`. **V-docs**
- **Non-interactive invocation:** "User-invoked skills and custom commands work. Include `/skill-name` in the prompt string and Claude Code expands it before running." **V-docs** https://code.claude.com/docs/en/headless (Note under "Create a commit"). The model can also invoke a skill automatically through the Skill tool, unless the skill sets `disable-model-invocation: true`.
- `--disable-slash-commands` disables all skills. `--bare` skips skill discovery (but "Skills still resolve via /skill-name" per 2.1.231 help). **V-help**
- **Verification:** stream-json `system/init.skills[]` lists the loaded skills. The adapter can check that a requested skill is present at runtime. **V-docs**
- **U:** whether `/skill-name` expansion applies when the prompt arrives on stdin rather than as the positional argument.

### 2.5 Model and effort

- `--model <alias|full-name>`; aliases `default, best, fable, opus, sonnet, haiku, sonnet[1m], opus[1m], opusplan`. Precedence: `/model` > `--model` > `ANTHROPIC_MODEL` > settings `model` > `ANTHROPIC_DEFAULT_MODEL`. **V-docs** https://code.claude.com/docs/en/model-config; flag **V-help**
- `--effort low|medium|high|xhigh|max` (2.1.231 help). Newer versions add `ultracode`. Env `CLAUDE_CODE_EFFORT_LEVEL`; settings `effortLevel` / `modelSettings`. Not persisted. **V-help, V-docs**
- Effort support depends on the model. An unsupported level "falls back to the highest supported level at or below the one you set". **V-docs**. So effort never makes validation fail on Claude.

### 2.6 Interactive resume (takeover)

- `claude --resume <session-id>`. "Claude Code leaves sessions created with `claude -p` … out of the session picker and out of `claude --continue`. You can still resume one by passing its session ID." **V-docs** https://code.claude.com/docs/en/sessions#resume-a-session
- Run it from the worktree. Cross-project lookup also works by ID.
- On a terminal resume without `-p`, the session's permission mode is restored, **except** `bypassPermissions`, which falls back to the default starting mode. Pass `--permission-mode` explicitly. **V-docs**
- On Pro/Max, a session idle for more than about 1h and over 100k tokens opens a "Resume from summary" dialog at interactive resume. Takeover UX should mention it. **V-docs**
- Resuming the same session in two processes interleaves both into one transcript. Mergeyard must make sure the headless process has exited before takeover. **V-docs**

### 2.7 Permissions, network, env, instruction files

- **Starting mode for `-p`:** `default` in sessions that fetch feature flags. Anything that would prompt is **denied** in `-p` (no host). **V-docs** https://code.claude.com/docs/en/permission-modes#which-mode-a-session-starts-in
- **Options for unattended edits + tests:**
  - `--permission-mode bypassPermissions` / `--dangerously-skip-permissions`: everything runs, including writes to protected paths. Docs recommend it only for containers/VMs. It refuses to run as root/sudo. **V-docs, V-help**
  - `--permission-mode acceptEdits --allowedTools "Bash(...)"`: edits are allowed, but shell commands need allow rules (bare `Bash` allows all). Writes to protected paths (`.git`, `.claude`, `.husky`, `.vscode`, `.npmrc`, `.mcp.json`, …) still prompt, which means **denied** in `-p`. **V-docs**
  - `--permission-mode auto`: a classifier reviews actions. Requires a supported model; falls back to Manual when unavailable. **V-docs**
  - `--permission-prompts none` (≥2.1.259, **not** in 2.1.231) tells Claude not to retry denied calls. **V-docs**
- **Reads outside the working directories:** the phase input (`~/.mergeyard/runs/…/input.md`) lives outside the worktree. Pass `--add-dir <phase-dir>` or send the input on stdin. **V-docs** (cli-reference `--add-dir`)
- **Trust:** `-p` skips the workspace-trust dialog. Project `.claude/settings.json` hooks and `.mcp.json` servers still run. **V-help, V-docs**
- **Network:** no sandbox by default. The optional Bash sandbox has no network unless enabled. **V-docs**
- **Env:** `CLAUDE_CONFIG_DIR`, `CLAUDE_CODE_PROJECT_DIR_NAME`, `CLAUDE_CODE_EFFORT_LEVEL`, `ANTHROPIC_MODEL`, `CLAUDE_CODE_RESUME_INTERRUPTED_TURN`, `CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS` (default 10 min wait for background subagents in `-p`). **V-docs**
- **Instruction files:** CLAUDE.md (`./CLAUDE.md`, `./.claude/CLAUDE.md`, parents, `~/.claude/CLAUDE.md`, subdirs lazily). AGENTS.md is read **only when no CLAUDE.md/CLAUDE.local.md exists** in the cwd or its parents, and only on ≥2.1.277. Otherwise import it with `@AGENTS.md` in CLAUDE.md. **V-docs** https://code.claude.com/docs/en/memory#agents-md
- **Auth check:** `claude auth status` prints JSON and "Exits with code 0 if logged in, 1 if not". `authMethod` ∈ `none, claude.ai, oauth_token, api_key, …`. **V-docs, V-help**

---

## 3. Codex CLI (detailed)

### 3.1 Headless resume and session IDs

- **New:** `codex exec [OPTIONS] [PROMPT]`. If the prompt is `-` or omitted, it is read from stdin. If stdin is piped *and* a prompt is given, stdin is appended as a `<stdin>` block. **V-help 0.156.1**
- **Resume:** `codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]`, with `--last` and `--all`. A PROMPT of `-` reads stdin. **V-help**. A parsable UUID is used directly, with no cwd filter. Names and `--last` are cwd-filtered unless `--all`. **V-src** `exec/src/lib.rs::resolve_resume_thread_id`
- **Option placement (important):** only some exec options are `global` and can follow `resume`: `--json`, `-o`, `--output-schema`, `--ephemeral`, `--skip-git-repo-check`, `--ignore-user-config`, `--ignore-rules`, `-m`, `--dangerously-bypass-approvals-and-sandbox`, `--worktree`, `-c`, `--enable/--disable`. `-s/--sandbox`, `-C/--cd`, `--add-dir`, `-p/--profile`, and `--oss` are **not** listed under `exec resume --help`. Put them **before** `resume`, or use `-c sandbox_mode=…`. **V-help, V-src** `exec/src/cli.rs`. A resume rebuilds config and sends the resolved sandbox and approval with `thread/resume` **V-src**. That a resumed thread actually adopts the new sandbox/model/effort is **U** (live test).
- **Session ID source:**
  - JSONL: the first event is `{"type":"thread.started","thread_id":"<uuid>"}`. It is emitted for both new and resumed threads, from `session_configured.thread_id`. **V-src** (`event_processor_with_jsonl_output.rs`), **V-docs** https://developers.openai.com/codex/noninteractive (→ learn.chatgpt.com/docs/non-interactive-mode)
  - Disk: `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<local-ts>-<thread_id>.jsonl`. The first line is `session_meta` with `id`, `cwd`, …. **V-local**. IDs are UUIDv7.
- **Pre-assigning an ID:** not supported. No flag in 0.156.1 help. **V-help**
- **Flags that must not be used:** `--ephemeral` ("Run without persisting session files to disk"). **V-help**
- `--worktree` can't be combined with `exec resume`. **V-src**. Mergeyard doesn't need it.
- **Resume-of-missing-ID error text:** **U** (not located).

### 3.2 Structured output

- `--json`: JSONL on stdout. Event types: `thread.started{thread_id}`, `turn.started`, `turn.completed{usage:{input_tokens,cached_input_tokens,cache_write_input_tokens,output_tokens,reasoning_output_tokens}}`, `turn.failed{error:{message}}`, `item.started|updated|completed{item:{id,type,…}}`, `error{message}`. Item types: `agent_message{text}`, `reasoning`, `command_execution`, `file_change`, `mcp_tool_call`, `collab_tool_call`, `web_search`, `todo_list`, `error`. **V-src** `exec/src/exec_events.rs`
- `--output-schema <FILE>`: "Path to a JSON Schema file describing the model's final response shape". **V-help**. Requests go out with `strict: true` by default (`Prompt::output_schema_strict = true`). **V-src** `core/src/client_common.rs`. So OpenAI strict-mode rules apply: every property listed in `required`, `additionalProperties:false`, optional values written as `"type":["string","null"]`. The docs say the schema "must define required properties and `additionalProperties: false`". **V-docs**
- The `agent_message.text` of the final message is "a JSON string when structured output is requested". **V-src**
- `-o/--output-last-message <FILE>`: written at shutdown **only if the turn completed**. On `turn.failed` / interrupted, the file is not written. With no agent message, it writes empty content plus a warning. **V-src** (`print_final_output`, `handle_last_message`). Delete stale files before each attempt (Mergeyard paths are per-attempt already).
- stdout carries JSONL or the final message. stderr carries progress. **V-docs**

### 3.3 Usage limits

- **Exit code:** `1` when a non-retrying `Error` notification arrives for the turn, or the turn ends `Failed`/`Interrupted`. Otherwise `0`. **V-src** `exec/src/lib.rs` (`error_seen` → `std::process::exit(1)`). Other `exit(1)` paths exist (config, not a git repo), so the exit code alone is not a classifier.
- **JSONL:** `{"type":"error","message":…}` then `{"type":"turn.failed","error":{"message":…}}`. **Only the message string** is exposed. There's no error code and no reset epoch in `--json`. **V-src**
- **Message templates (0.156.1, `protocol/src/error.rs`, `UsageLimitReachedError`)**, by plan:
  - Plus: `You’ve hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at <T>.`
  - Pro/ProLite: `You’ve hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at <T>.`
  - Team/Business/Enterprise-CBP: `You’ve hit your usage limit. To get more access now, send a request to your admin or try again at <T>.`
  - Free/Go: `You’ve hit your usage limit. Upgrade to Plus to continue using Codex (https://chatgpt.com/explore/plus), or try again at <T>.`
  - Enterprise/Edu/unknown: `You’ve hit your usage limit. Try again at <T>.`
  - Per-model limit: `You’ve hit your usage limit for <limit_name>. Switch to another model now, or try again at <T>.`
  - Without a reset time: `… or try again later.` / `Try again later.`
  - Non-time-bound variants: `Your workspace is out of credits. …`, `You hit your spend cap set in your workspace. …`
  - `main` (2026-10-03) already changed the URLs to `https://chatgpt.com/settings/usage`. **Match on the stable prefix `hit your usage limit`, never on the full string.**
  - The source uses U+2019 (`You’ve`). Older logs on this machine show ASCII `You've`. Match both. **V-src, V-local**
- **Reset-time format `<T>`:** machine-local time. Same day: `%-I:%M %p` (e.g. `5:19 PM`). Other day: `%b %-d<st|nd|rd|th>, %Y %-I:%M %p` (e.g. `Oct 3rd, 2026 6:23 PM`). **V-src** `format_retry_timestamp`, **V-local**
- **Machine-readable fallback (V-local):** the rollout file for the thread holds:
  - `event_msg/task_complete` with `"error":{"message":…,"codex_error_info":"usage_limit_exceeded"}`
  - `event_msg/token_count` with `"rate_limits":{"limit_id":"codex","primary":{"used_percent":100.0,"window_minutes":10080,"resets_at":1791048197},"secondary":…,"plan_type":"prolite",…}`. `resets_at` is epoch seconds: 1791048197 = 2026-10-03 17:23:17Z, which matches the message `Oct 3rd, 2026 6:23 PM` in Europe/Lisbon. `window_minutes` 300 is the 5-hour window and 10080 the weekly one.
  - The rollout format is internal (there is even a `migrate-rollouts` command), so treat it as best-effort.
- Other codes in `CodexErrorInfo`: `rate_limit_exceeded` (429 throttling), `server_overloaded`, `context_window_exceeded`, … **V-src**

### 3.4 Skills

- **Locations, in search order:** `$CWD/.agents/skills`, `$REPO_ROOT/.agents/skills`, `$HOME/.agents/skills`, `/etc/codex/skills`. System skills are cached under `$CODEX_HOME/skills/.system`. **V-docs** https://developers.openai.com/codex/skills (→ learn.chatgpt.com/docs/build-skills); `.system` **V-src** `skills/src/lib.rs`; `~/.agents/skills/<name>/SKILL.md` **V-local**.
- **Format:** `SKILL.md` with frontmatter `name`, `description`. Optional `scripts/`, `references/`, and `agents/openai.yaml` metadata. Can be disabled with `[[skills.config]]` in config.toml. Same-name skills are not merged. **V-docs**
- **Invocation:** an explicit `$skill-name` text mention. `collect_explicit_skill_mentions` scans every text `UserInput` in `core/src/session/turn.rs`, which is shared by TUI and exec, so it works in `codex exec` and in stdin prompts. Plain names resolve only when unambiguous. **V-src**. Implicit selection by description also works. **V-docs**. Live confirmation in exec is **U**.
- `.agents` is a protected path under writable roots, so agents can't modify skills in workspace-write. **V-src** `protocol/src/permissions.rs`

### 3.5 Model and effort

- `-m, --model <MODEL>` on `exec`, `exec resume`, `resume`. **V-help**
- Effort: `-c model_reasoning_effort=<level>`; there is no dedicated flag. Config reference: "Reasoning effort advertised by the selected model, such as `low`, `medium`, `high`, `xhigh`, `max`, or `ultra`." **V-docs** https://developers.openai.com/codex/config-reference. Per-model support differs (local `models_cache.json`: `gpt-6.1-sol` supports `low…ultra`; `gpt-5.5` only `low…xhigh`). **V-local**. Behavior for an unsupported level is **U**.
- Profiles: `-p <name>` layers `$CODEX_HOME/<name>.config.toml`. **V-help**

### 3.6 Interactive resume (takeover)

- `codex resume [SESSION_ID] [PROMPT]`; `--last`; `--all`; `--include-non-interactive` ("Include non-interactive sessions in the resume picker and --last selection"). **V-help**
- An explicit UUID goes straight to `thread_read` with no source-kind or cwd filter, so exec-created threads resume interactively by ID. **V-src** `tui/src/lib.rs::lookup_session_target_with_app_server`
- cwd: passing `-C <dir>` forces "current" resume-cwd mode. Otherwise the `tui.resume_cwd` setting applies. Launch from the worktree (or with `-C <worktree>`). **V-src** `tui/src/session_resume.rs`
- The TUI accepts `-s`, `-a on-request|never`, `-m`, `-c`. **V-help**

### 3.7 Permissions, sandbox, network, env, instruction files

- **Approval:** `exec` hard-sets `approval_policy = Never` ("Default to never ask for approvals in headless mode") unless the configured approvals reviewer is AutoReview. `exec` has no `-a` flag. **V-src** `exec/src/lib.rs:566`, **V-help**
- **Sandbox:** `-s read-only|workspace-write|danger-full-access`. The exec default is read-only ("Default: Read-only sandbox (no edits)"). **V-docs, V-help**. `--dangerously-bypass-approvals-and-sandbox` means DangerFullAccess and also skips the git-repo check. **V-src**
- **`--full-auto`:** "Deprecated; use `--sandbox workspace-write` instead". Absent from 0.156.1 help. **V-docs, V-help**
- **Network:** `sandbox_workspace_write.network_access` is a bool, default false (serde default). Enable it with `-c sandbox_workspace_write.network_access=true` when tests or installs need the network. **V-docs, V-src** `config/src/types.rs`
- **Protected metadata under writable roots:** `.git`, `.agents`, `.codex`. **V-src** `protocol/src/permissions.rs`
  - In a *linked* worktree, git metadata lives in `<base>/.git/worktrees/<run-id>/`, which is outside the writable root. Agent-side `git add`/`git commit` will probably fail under workspace-write (**U**, live test). Mergeyard commits itself (PRD §16), so instruct agents not to commit.
  - The `result.json` fallback path under `~/.mergeyard/runs/…` is also outside the writable roots, so the agent can't write it unless you pass `--add-dir <phase-dir>`.
- **Git requirement:** exits `1` with `Not inside a trusted directory and --skip-git-repo-check was not specified.` when the cwd isn't in a git repo. **V-src**. Worktrees satisfy this.
- **Env:** `CODEX_HOME` (config, auth, sessions). `CODEX_API_KEY` is for API-key CI runs. **V-help, V-docs**
- **Instruction files:** `~/.codex/AGENTS.override.md` or `AGENTS.md` (global), then from the repo root down to the cwd, one file per directory (`AGENTS.override.md` > `AGENTS.md` > fallbacks), capped at 32 KiB (`project_doc_max_bytes`). CLAUDE.md is **not** read unless `project_doc_fallback_filenames = ["CLAUDE.md"]`, and only in directories without an AGENTS.md. **V-docs** https://developers.openai.com/codex/guides/agents-md
- **Auth check:** `codex login status` exits 0 when logged in (ChatGPT/API key/token) and 1 for `Not logged in` or an error. Output goes to stderr. **V-src** `cli/src/login.rs`
- `codex exec review` (`--base`, `--commit`, `--uncommitted`) exists as a built-in reviewer. It isn't recommended: it starts its own thread and wouldn't follow Mergeyard's review contract. **V-help**

---

## 4. Open questions that need a live (paid) test

1. **Claude `-p` usage limit:** the exact `--output-format json`/`stream-json` result (`is_error`, `subtype`, `result` text, `api_error_status` 429?), the exit code, whether `rate_limit_event{status:"rejected"}` is emitted, and the unit of `resetsAt`. Also whether the weekly string includes a weekday.
2. **Codex usage limit in `exec --json`:** confirm `error` + `turn.failed` events, exit 1, and that the rollout `token_count.rate_limits` is written before the failure when the limit is hit on the first request.
3. **Claude `/skill-name` expansion:** does it work when the prompt comes on stdin, or combined with `--json-schema`? Can more than one skill be named?
4. **Codex `$skill` mention in `exec`:** confirm it injects the skill (check the rollout or JSONL items).
5. **Claude `--json-schema` together with `--resume`:** confirm `structured_output` is populated on resumed sessions, and that re-prompting on mismatch doesn't burn many turns.
6. **Codex resume overrides:** confirm `codex exec -s workspace-write -c model_reasoning_effort=high resume <id> -` applies the new sandbox/effort/model to the resumed thread, and that `--output-schema` works on resume.
7. **Resume-of-unknown-ID:** Codex message and exit code. Confirm Claude prints `No conversation found with session ID:` with a non-zero exit in `-p` JSON mode.
8. **Codex in a linked worktree under workspace-write:** do `git status`/`git diff` work, and does `git commit` fail? Same check for build tools that write outside the worktree (`~/.cache`, `~/.npm`).
9. **Signals:** Codex behavior and exit code on SIGINT/SIGTERM mid-turn, and whether the thread can be resumed afterwards. Claude: SIGINT exit code.
10. **Handback continuity:** after `claude --resume <id>` interactively, the next `claude -p --resume <id>` sees the manual turns (also check the case where the user runs `/clear` or `/branch`, which changes the ID). Same for `codex resume <id>` then `codex exec resume <id>`.
11. **Claude 2.1.231 specifics:** whether `stream-json` requires `--verbose`, and whether the `json` result includes `structured_output`/`terminal_reason`. Running `claude update` to ≥2.1.277 may be preferable before the spikes.
12. **Codex strict-schema rejection:** the error text when a non-strict schema is passed (for example an optional property). This confirms the validation strategy.

---

## 5. Recommendations

### 5.1 General adapter rules

- **Prompt delivery:** a short, Mergeyard-authored, trusted positional prompt plus the phase input file. Both harnesses can read the file once access is granted: Claude needs `--add-dir <phase-dir>`, while Codex can read anywhere by default. Alternatively, pipe `input.md` on stdin (Claude caps stdin at 10 MB). Untrusted text never goes into argv (PRD §13.1, §32 stay intact).
- **stdout/stderr:** keep stdout as a clean event stream (`events.jsonl`) and stderr separate. Both adapters parse stdout.
- **Persist the session ID early.** Claude: Mergeyard generates the UUID and passes `--session-id`. Codex: tail `events.jsonl` for `thread.started` and persist `thread_id` immediately, before the phase finishes (needed for crash/usage-limit resume).
- **Native structured output everywhere.** Drop the `result.json` prompt fallback for these two adapters (it conflicts with Codex's sandbox and Claude's out-of-cwd write prompts). Write each phase schema in OpenAI strict form so one schema file serves both harnesses: every property required, `additionalProperties:false`, nullable instead of optional.
- **Never use** Claude `--bare`, `--no-session-persistence`, `--continue`; Codex `--ephemeral`, `--last`.
- **Re-pass all config flags on every resume.** Claude doesn't restore permission mode, `--add-dir`, `--settings`, or `--mcp-config` in `-p`.

### 5.2 Claude adapter templates

```sh
# implement (new implementer session) — Mergeyard generates $SID (UUIDv4) and stores it before launch
cd "$WORKTREE" && claude -p \
  --session-id "$SID" \
  --output-format stream-json --verbose \
  --json-schema "$IMPLEMENT_SCHEMA_JSON" \
  --permission-mode "$PERMISSION_MODE" \
  --add-dir "$PHASE_DIR" \
  [--model "$MODEL"] [--effort "$EFFORT"] \
  "Read $PHASE_DIR/input.md and follow it. Do not commit." \
  < /dev/null > "$PHASE_DIR/events.jsonl" 2> "$PHASE_DIR/stderr.log"

# fix (resume implementer)  — same flags, --resume instead of --session-id, $FIX_SCHEMA_JSON
claude -p --resume "$IMPL_SID" --output-format stream-json --verbose --json-schema "$FIX_SCHEMA_JSON" \
  --permission-mode "$PERMISSION_MODE" --add-dir "$PHASE_DIR" [--model …] [--effort …] \
  "Read $PHASE_DIR/input.md and follow it. Do not commit."

# review (new reviewer session on round 1: --session-id "$REV_SID"; later rounds: --resume "$REV_SID")
#   with a skill: prompt = "/$SKILL Read $PHASE_DIR/input.md and follow it."   (explicit expansion)
#   optional read-only hardening: --disallowedTools "Edit" "Write" "NotebookEdit"
claude -p --session-id "$REV_SID" --output-format stream-json --verbose --json-schema "$REVIEW_SCHEMA_JSON" \
  --permission-mode "$PERMISSION_MODE" --add-dir "$PHASE_DIR" [--model …] [--effort …] \
  "/$SKILL Read $PHASE_DIR/input.md and follow it."

# takeover (interactive)
cd "$WORKTREE" && claude --resume "$IMPL_SID" [--permission-mode acceptEdits]
```

Result parsing: take the last `type:"result"` line. Then:
- `is_error:false` and `structured_output` present → validate against the Mergeyard schema.
- `is_error:true` → check usage limit (below), then generic failure.

Usage-limit classifier (Claude), any of:
- a `rate_limit_event` with `rate_limit_info.status=="rejected"` (reset from `resetsAt`)
- result/assistant `api_error_status==429` / `error=="rate_limit"`
- text matching `/You've hit your (session|weekly|\w+) limit · resets (?:(Mon|Tue|Wed|Thu|Fri|Sat|Sun) )?(\d{1,2}(?::\d{2})?(?:am|pm))(?: \(([^)]+)\))?/`

Treat `spend limit`, `credits_required`, `usage credits` as **not** time-bound. Use the cooldown, or `NEEDS_ATTENTION`.

`$PERMISSION_MODE` default: see PRD change 6. `bypassPermissions` is the only mode that guarantees unattended test runs without per-command allow-lists. `auto` is the safer alternative but can silently deny commands in `-p`.

### 5.3 Codex adapter templates

```sh
# implement (new implementer thread); thread_id taken from first JSONL event
codex exec --json -o "$PHASE_DIR/last-message.json" --output-schema "$PHASE_DIR/schema.json" \
  -s workspace-write -c sandbox_workspace_write.network_access=true \
  -C "$WORKTREE" [-m "$MODEL"] [-c model_reasoning_effort="$EFFORT"] \
  "Read $PHASE_DIR/input.md and follow it. Do not commit." \
  < /dev/null > "$PHASE_DIR/events.jsonl" 2> "$PHASE_DIR/stderr.log"

# fix (resume implementer) — non-global options BEFORE `resume`
codex exec -s workspace-write -c sandbox_workspace_write.network_access=true -C "$WORKTREE" \
  resume "$IMPL_THREAD_ID" \
  --json -o "$PHASE_DIR/last-message.json" --output-schema "$PHASE_DIR/schema.json" \
  [-m "$MODEL"] [-c model_reasoning_effort="$EFFORT"] \
  "Read $PHASE_DIR/input.md and follow it. Do not commit."

# review (round 1 new, later rounds `resume "$REV_THREAD_ID"`); skill via $mention
#   optional enforcement: -s read-only (blocks writes, incl. test caches — Mergeyard restores anyway)
codex exec --json -o … --output-schema … -s workspace-write -C "$WORKTREE" [-m …] [-c model_reasoning_effort=…] \
  "\$$SKILL Read $PHASE_DIR/input.md and follow it."

# takeover (interactive)
codex resume -C "$WORKTREE" "$IMPL_THREAD_ID"   # or: cd "$WORKTREE" && codex resume "$IMPL_THREAD_ID"
```

Result parsing:
- Success means exit 0 **and** a `turn.completed` event. The structured result is `last-message.json`, or the last `item.completed` with `item.type=="agent_message"` (`text` is a JSON string).
- Failure means exit ≠ 0 or `turn.failed`.

Usage-limit classifier (Codex):
- `turn.failed.error.message` or `error.message` matches `/hit your usage limit/i` (curly or straight apostrophe).
- Reset time, in order of preference:
  1. The rollout file `$CODEX_HOME/sessions/**/rollout-*-<thread_id>.jsonl`. Take the last `token_count.rate_limits.{primary,secondary}` entry with `used_percent>=100` and use its `resets_at` (epoch s). Confirm with `task_complete.error.codex_error_info=="usage_limit_exceeded"`.
  2. Otherwise parse `try again at (\d{1,2}:\d{2} [AP]M)` (today, local) or `try again at ([A-Z][a-z]{2} \d{1,2}(st|nd|rd|th), \d{4} \d{1,2}:\d{2} [AP]M)`.
  3. Otherwise use the cooldown.
- Treat `out of credits` / `spend cap` as not time-bound.

### 5.4 PRD changes these findings imply

1. **§11 Agent sessions:** specify where each harness gets its session ID. Claude: Mergeyard pre-assigns a UUID with `--session-id` on the first phase and uses `--resume` afterwards. Codex: the adapter reads `thread.started.thread_id` from the live JSONL stream and persists it as soon as it appears. Add a `session_id_source` note to `HarnessCapabilities` (pre-assigned vs discovered).
2. **§11 resume-failure detection:** name the signals. Claude: `No conversation found with session ID:`. Codex: TBD (open question 7). Add that Claude transcripts age out after `cleanupPeriodDays` (30 d by default), which is a known cause of resume failure.
3. **§13.2 Result:** make native structured output the primary path for both MVP adapters (Claude `--json-schema` → `structured_output`; Codex `--output-schema` + `-o`). Demote or remove the `result.json` fallback: Codex's workspace-write sandbox and Claude's out-of-cwd write rule both block writes to `~/.mergeyard/runs/…`. Require phase schemas to be OpenAI-strict-compatible: all fields required, `additionalProperties:false`, nullable instead of optional. For example, review `findings[].file` and `.line` become `["string","null"]` / `["integer","null"]`. Add `schema_version` as a `const`.
4. **§13.1 Input:** state that the agent gets access to the phase input via Claude `--add-dir <phase-dir>` (or stdin) and Codex default read access. The positional prompt is a fixed Mergeyard template containing only Mergeyard-generated paths and skill names.
5. **§14 Phase wrapper:** step 4 should write stdout (the machine event stream) and stderr to **separate** files (`events.jsonl`, `stderr.log`), plus a combined human log if desired. The adapter parses stdout, and the Codex adapter must tail it during the run for `thread.started`.
6. **§26 Configuration and §32 Security:** add per-role harness permission settings, because neither harness runs unattended with edits and tests by default:
   - `claude.permission_mode`: proposed default `bypassPermissions`, consistent with §32's "Mergeyard does not sandbox agent code". Allowed values `auto | acceptEdits | bypassPermissions`, plus optional `allowed_tools`.
   - `codex.sandbox`: default `workspace-write`; plus `codex.network_access` (default `true`, since tests often need it; workspace-write's built-in default is no network).
   - Note that Claude refuses `bypassPermissions` as root.
7. **§12 Usage limits, Detection:** record the concrete patterns and reset sources from §5.2/§5.3. Add that reset times may be **local wall-clock without a date** (Claude: `resets 5pm (Europe/Lisbon)`; Codex same-day: `5:19 PM`). Parse them as the next occurrence in the stated or machine time zone. Add that spend-cap / credits-exhausted messages are **not** time-bound usage limits; they should go straight to `NEEDS_ATTENTION` or use the cooldown, and Mergeyard should not wait until a reset time. Prefer structured sources: Claude `rate_limit_event.resetsAt` (requires the stream-json output format, so make stream-json the Claude adapter default) and Codex rollout `rate_limits.*.resets_at`.
8. **§11 Capabilities / §30 Doctor, skills:**
   - Claude skill lookup paths: `~/.claude/skills`, `<repo>/.claude/skills`, plugin skills (doctor reports these as unverifiable). Runtime confirmation comes from stream-json `system/init.skills`.
   - Codex skill lookup paths: `<repo>/.agents/skills`, `~/.agents/skills`, `/etc/codex/skills`.
   - Invocation: Claude `/skill-name` at the start of the prompt; Codex `$skill-name` mention.
   - Multiple skills per role are guaranteed for Codex but only "by instruction" for Claude after the first (pending open question 3).
9. **§30 Doctor:**
   - Use `claude auth status` (exit 0/1, JSON) and `codex login status` (exit 0/1) as the "logged in" checks.
   - Add minimum-version checks: Claude ≥2.1.223 (resume by ID across dirs) and ≥2.1.277 if AGENTS.md support matters. The installed 2.1.231 doesn't read AGENTS.md.
   - Warn when a repo has only `CLAUDE.md` and Codex is assigned (Codex ignores it without `project_doc_fallback_filenames`), or only `AGENTS.md` with Claude <2.1.277.
10. **§13.1 last paragraph:** "The harness consumes them naturally" is asymmetric. Document the rule: Codex reads AGENTS.md only; Claude reads CLAUDE.md, and reads AGENTS.md only without a CLAUDE.md (≥2.1.277).
11. **§16 Git:** add "Agent prompts instruct harnesses not to commit". Codex's workspace-write sandbox protects `.git`, and in a linked worktree the git metadata sits outside the writable root, so agent commits are likely to fail (open question 8).
12. **§15 Review is read-only:** optionally enforce it at the harness level (Codex `-s read-only`; Claude `--disallowedTools Edit Write NotebookEdit`) on top of the restore step. Keep the restore as the guarantee.
13. **§20 Takeover:**
    - Templates are `cd <worktree> && claude --resume <id>` and `codex resume -C <worktree> <id>`. Headless sessions don't appear in either harness's picker, so the dashboard must show the exact ID.
    - Step 1 ("stops gracefully") should send **SIGINT** first. Claude SIGTERM exits 143 and leaves the turn unfinished.
    - Claude does not restore `bypassPermissions` on interactive resume.
    - Handback: if the user branches or clears the Claude conversation, the session ID changes. Mergeyard keeps resuming the original ID.
14. **§26 effort:** effort values are model-specific pass-through. Claude: `low…max` with silent downgrade. Codex: `low…max/ultra`, varies per model. Validation can only check that the harness supports the capability, not the value.
15. **§37:** mark spikes 1, 2, 4, 5, 6 as answered by this document. Spike 3 is partially answered: the open questions in §4 need a live run before the usage-limit classifier is final.
