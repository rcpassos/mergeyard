## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues on `rcpassos/mergeyard`, through the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The default five labels: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.

## Testing

Locally, run `make lint` and `go test -count=1 ./internal/scheduler/` for scheduler policy and journal changes. Real Git, tmux, and process-restart tests live in `internal/scheduler/integration/`; run the specific integration tests you changed with `go test -run '<TestName>' ./internal/scheduler/integration/`.

CI owns the full `go test ./...` suite and runs both packages in four shards. Verify a change by pushing it and reading `gh pr checks`.
