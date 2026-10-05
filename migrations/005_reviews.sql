-- Each review attempt has a pre-launch snapshot and a restoration journal.
-- No existing M1 run or attempt is rewritten.
CREATE TABLE review_attempts (
    attempt_id TEXT PRIMARY KEY NOT NULL REFERENCES phase_attempts(id),
    target_sha TEXT NOT NULL,
    diff TEXT NOT NULL,
    snapshot_json TEXT NOT NULL,
    session_id TEXT NOT NULL,
    permission_mode TEXT NOT NULL,
    allowed_tools_json TEXT NOT NULL,
    restoration_started INTEGER NOT NULL DEFAULT 0 CHECK (restoration_started IN (0,1)),
    restored INTEGER NOT NULL DEFAULT 0 CHECK (restored IN (0,1)),
    contaminated INTEGER NOT NULL DEFAULT 0 CHECK (contaminated IN (0,1)),
    accepted INTEGER NOT NULL DEFAULT 0 CHECK (accepted IN (0,1)),
    report_json TEXT
);
CREATE UNIQUE INDEX review_attempt_identity ON phase_attempts(run_id,phase,round,attempt)
    WHERE phase='review';
