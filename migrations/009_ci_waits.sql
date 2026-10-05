-- Add commit-specific waits without rewriting M1 runs, attempts, or events.
CREATE TABLE ci_waits (
    run_id TEXT PRIMARY KEY NOT NULL REFERENCES runs(id),
    snapshot_json TEXT NOT NULL
);
