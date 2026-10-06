-- Retry intent precedes observation; selection and grants commit with lifecycle.
CREATE TABLE run_retries (
    id INTEGER PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id),
    pending INTEGER NOT NULL CHECK (pending IN (0,1)),
    snapshot_json TEXT NOT NULL
);
CREATE UNIQUE INDEX one_pending_retry ON run_retries(run_id) WHERE pending=1;
