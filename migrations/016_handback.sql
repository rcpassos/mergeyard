-- Handback publication intent and round selection survive interrupted requests.
CREATE TABLE run_handbacks (
    id INTEGER PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id),
    pending INTEGER NOT NULL CHECK (pending IN (0,1)),
    snapshot_json TEXT NOT NULL
);
CREATE UNIQUE INDEX one_pending_handback ON run_handbacks(run_id) WHERE pending=1;
