CREATE TABLE harness_checks (
 id TEXT PRIMARY KEY NOT NULL,
 harness TEXT NOT NULL,
 restriction_id TEXT NOT NULL,
 request_json TEXT NOT NULL,
 process_session TEXT NOT NULL,
 phase_dir TEXT NOT NULL,
 status TEXT NOT NULL CHECK (status IN ('reserved','running','recovered','released')),
 result TEXT NOT NULL DEFAULT '',
 requested_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
 ended_at TEXT
);
ALTER TABLE credit_probes RENAME TO previous_credit_probes;
DROP INDEX credit_probe_owner;
DROP INDEX credit_probes_run;
CREATE TABLE credit_probes (
 id TEXT PRIMARY KEY NOT NULL,
 harness TEXT NOT NULL,
 restriction_id TEXT NOT NULL,
 run_id TEXT REFERENCES runs(id),
 attempt_id TEXT REFERENCES phase_attempts(id),
 check_id TEXT REFERENCES harness_checks(id),
 status TEXT NOT NULL CHECK (status IN ('reserved','running','recovered','released')),
 requested_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
 ended_at TEXT,
 CHECK ((run_id IS NOT NULL AND check_id IS NULL) OR (run_id IS NULL AND attempt_id IS NULL AND check_id IS NOT NULL))
);
INSERT INTO credit_probes(id,harness,restriction_id,run_id,attempt_id,status,requested_at,ended_at)
 SELECT id,harness,restriction_id,run_id,attempt_id,status,requested_at,ended_at FROM previous_credit_probes;
DROP TABLE previous_credit_probes;
CREATE UNIQUE INDEX credit_probe_owner ON credit_probes(harness) WHERE status IN ('reserved','running');
CREATE INDEX credit_probes_run ON credit_probes(run_id,requested_at);
