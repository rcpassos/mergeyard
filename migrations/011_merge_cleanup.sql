-- Maintenance survives coding completion and does not consume active capacity.
CREATE TABLE merge_cleanup (
    run_id TEXT PRIMARY KEY NOT NULL REFERENCES runs(id),
    snapshot_json TEXT NOT NULL
);
