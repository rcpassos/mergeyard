-- A verified missing startup does not spend a normal attempt. The unique journal
-- is committed with its replacement attempt before any process is launched.
ALTER TABLE phase_attempts ADD COLUMN session_id TEXT;
CREATE TABLE session_recoveries (
    run_id TEXT NOT NULL REFERENCES runs(id),
    phase TEXT NOT NULL,
    role TEXT NOT NULL,
    round INTEGER NOT NULL,
    failed_attempt_id TEXT NOT NULL UNIQUE REFERENCES phase_attempts(id),
    replacement_attempt_id TEXT NOT NULL UNIQUE REFERENCES phase_attempts(id),
    previous_session_id TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (run_id, phase, role, round)
);
