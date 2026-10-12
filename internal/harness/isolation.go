package harness

// These standing instructions apply equally to fresh and resumed agent phases.
const isolationInstructions = `## Run isolation

The wrapper exports MERGEYARD_RUN_ID (this run), MERGEYARD_PHASE (implement,
review, or fix), MERGEYARD_PORT_BASE (the first port assigned to this phase),
and MERGEYARD_PORT_COUNT (the number of consecutive assigned ports).
Use only ports from MERGEYARD_PORT_BASE through
MERGEYARD_PORT_BASE + MERGEYARD_PORT_COUNT - 1 for dev servers, test databases,
and other services. Namespace browser automation and test sessions with
MERGEYARD_RUN_ID; add MERGEYARD_PHASE so independent roles do not share sessions.
Follow the repository's AGENTS.md / CLAUDE.md for how to start and configure
services using these values. Verify against your own service and clean up your
owned services and sessions when finished.

`
