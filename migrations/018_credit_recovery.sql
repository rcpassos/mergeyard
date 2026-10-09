-- Preserve timed restrictions and their foreign keys while allowing no reset.
ALTER TABLE harness_limits RENAME TO previous_harness_limits;
CREATE TABLE harness_limits (
 harness_type TEXT PRIMARY KEY NOT NULL,
 limited_until TEXT,
 reset_time_source TEXT NOT NULL DEFAULT '' CHECK (reset_time_source IN ('', 'reported', 'default_cooldown')),
 detected_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
 phase_attempt_id TEXT REFERENCES phase_attempts(id),
 signal_source TEXT NOT NULL DEFAULT '',
 notified_until TEXT NOT NULL DEFAULT '',
 reason TEXT NOT NULL DEFAULT 'temporary_limit' CHECK (reason IN ('temporary_limit','credits_exhausted')),
 restriction_id TEXT NOT NULL DEFAULT '',
 probe_id TEXT NOT NULL DEFAULT ''
);
INSERT INTO harness_limits(harness_type,limited_until,reset_time_source,detected_at,phase_attempt_id,signal_source,notified_until)
 SELECT harness_type,limited_until,reset_time_source,detected_at,phase_attempt_id,signal_source,notified_until FROM previous_harness_limits;
DROP TABLE previous_harness_limits;
CREATE TABLE credit_probes (
 id TEXT PRIMARY KEY NOT NULL,
 harness TEXT NOT NULL,
 restriction_id TEXT NOT NULL,
 run_id TEXT NOT NULL REFERENCES runs(id),
 attempt_id TEXT REFERENCES phase_attempts(id),
 status TEXT NOT NULL CHECK (status IN ('reserved','running','recovered','released')),
 requested_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
 ended_at TEXT
);
CREATE UNIQUE INDEX credit_probe_owner ON credit_probes(harness) WHERE status IN ('reserved','running');
CREATE INDEX credit_probes_run ON credit_probes(run_id,requested_at);
