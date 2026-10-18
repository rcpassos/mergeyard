-- Body publication records one bounded row per run: the latest evidence whose
-- PR body update was attempted, its attempt count and time, and whether the
-- last returned attempt failed. A failing PR must not add an event row per
-- tick; the event stream keeps only completions, warnings and receipts.
CREATE TABLE verification_attempts (
    run_id TEXT PRIMARY KEY NOT NULL REFERENCES runs(id),
    source_event INTEGER NOT NULL REFERENCES events(id),
    attempts INTEGER NOT NULL DEFAULT 1 CHECK (attempts > 0),
    last_attempt_at TEXT NOT NULL,
    failed INTEGER NOT NULL DEFAULT 0 CHECK (failed IN (0, 1))
);
