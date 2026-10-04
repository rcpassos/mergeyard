# Interactive setup

Run `mergeyard init` with `git`, `gh`, `tmux`, and at least one of Claude Code
(`claude`) or Codex (`codex`) on your `PATH`. Authenticate with GitHub and the
agent beforehand. Init reports installed tools, offers installed agents for each
role, and asks for the first repository as `owner/repo`.

Init follows the normal configuration search order: `--config <path>`, then
`./mergeyard.yaml`, then `~/.config/mergeyard/config.yaml`. On first setup with
no explicit path and no existing file, it creates the home config. It asks before
updating an existing file. Declining leaves the file untouched. An existing file
must parse successfully to be updated.

On re-running init, the selected agents update the global implementer and reviewer
settings. Existing repository overrides, settings, comments, and unknown fields
are preserved. The entered repository is added if it is not already configured;
other repositories remain. YAML whitespace may be normalized by the config writer.

After verifying GitHub access, init offers to create missing labels. Confirmation
is required; the default is no. Existing labels are matched ignoring case and
retain their colors and descriptions. New configurations use `ready-for-agent`,
`agent-running`, and `agent-needs-attention`. Existing configurations use the
selected repository's effective labels, including overrides.

Init saves the config and runs `doctor`, printing its grouped findings. It returns
nonzero if doctor finds errors, while leaving the saved config available for
correction. Login, version, permissions, and repository access problems appear
in that report. If labels were declined and are absent, doctor reports them.

## Adding repositories

Run `mergeyard repo add owner/repo` to append to the selected configuration. It
requires an existing valid config and verifies GitHub access before saving. It
rejects invalid names and repositories already configured, ignoring case. It
offers to create missing labels using the global label settings inherited by the
new repository. Declining label creation still adds the repository. Run
`mergeyard doctor` afterward to check its remaining prerequisites.

Both commands keep the config unchanged on incomplete input or failures before
saving. Label creation can partially succeed before a later label or config write
fails; re-running is safe because existing labels are skipped. External GitHub
commands have a 30-second timeout per command.

## Current startup limit

The setup commands generate configuration without hand-editing YAML. Starting a
running dashboard and scheduler remains deferred to issue #14 by the current
`start` implementation; the complete PRD §39 setup acceptance flow cannot yet be
verified end to end.
