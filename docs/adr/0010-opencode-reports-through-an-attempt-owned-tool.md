# OpenCode reports through an attempt-owned tool

Released OpenCode V2 does not establish a native schema-constrained final-message contract equivalent to the existing Claude Code and Codex integrations. For OpenCode, allow an adapter-owned typed reporting tool that writes to a fixed, attempt-owned artifact path outside the worktree. Mergeyard validates its output with the canonical phase schema and semantic checks, and requires independent native completion evidence before an attempt can succeed.

This is an explicit exception to the PRD's native structured-output transport requirement. It retains the phase-result contract while allowing the agreed V2 baseline. Missing, malformed, or stale reports fail the attempt; an emitted report does not prove native completion. Paths and attempt identities belong to Mergeyard rather than model arguments, and plugin-side argument validation never replaces canonical validation.

The mechanism requires compatibility evidence before implementation is considered supported: plugin loading, schema compatibility, invalid-argument correction, duplicate/stale reports, and interrupted/resumed execution must be verified. The existing Claude Code and Codex report transports remain in use.
