ALTER TABLE harness_limits ADD COLUMN signal_source TEXT NOT NULL DEFAULT '';
ALTER TABLE harness_limits ADD COLUMN notified_until TEXT NOT NULL DEFAULT '';
CREATE TABLE harness_waits (
 id INTEGER PRIMARY KEY,
 run_id TEXT NOT NULL REFERENCES runs(id),
 phase_attempt_id TEXT UNIQUE REFERENCES phase_attempts(id),
 snapshot_json TEXT NOT NULL
);
CREATE INDEX harness_waits_run ON harness_waits(run_id, id);
