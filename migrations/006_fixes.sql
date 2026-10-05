-- Keep every attempt's input and report, and pin a commit before pushing it.
CREATE TABLE fix_attempts (
    attempt_id TEXT PRIMARY KEY NOT NULL REFERENCES phase_attempts(id),
    session_id TEXT NOT NULL,
    target_sha TEXT NOT NULL,
    findings_json TEXT NOT NULL,
    permission_mode TEXT NOT NULL,
    allowed_tools_json TEXT NOT NULL,
    report_json TEXT,
    commit_sha TEXT,
    pushed INTEGER NOT NULL DEFAULT 0 CHECK (pushed IN (0,1))
);
CREATE UNIQUE INDEX fix_attempt_identity ON phase_attempts(run_id,phase,round,attempt)
    WHERE phase='fix';
