# Role settings are resolved at claim and fixed for the run

Per-issue harness overrides (#117, the first slice of #102) mean a run's role settings can no longer be read from repository configuration at each phase. Each role's harness, model, effort and skills are therefore resolved once, when the issue is claimed, stored on the run, and used by every later phase, restart and resume of that run. Configuration changes apply only to runs claimed afterwards. Runs created before this decision, which have no stored role settings, keep reading repository configuration.

## Considered Options

- **Store only the harness and keep reading model and effort from configuration.** Rejected: a configuration edit could then change the model of a resumed session in the middle of a run, and one run would mix two sources of truth.
- **Re-resolve role settings at every phase (the previous behaviour).** Rejected: it cannot represent a per-issue override, and a configuration change to a role's harness already strands the run with `harness.session_agent_changed`.

## Consequences

- Changing a harness override label after claim has no effect on that run. Moving a run to another harness mid-run is out of scope, because an implementer session belongs to one harness.
- Diagnostics such as `mergeyard plan` and the run detail must show the stored role settings, not the current configuration.
