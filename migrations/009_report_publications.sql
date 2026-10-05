-- Frozen comment bodies and remote receipts survive workflow completion/restart.
CREATE TABLE report_publications (
    attempt_id TEXT PRIMARY KEY NOT NULL REFERENCES phase_attempts(id),
    run_id TEXT NOT NULL REFERENCES runs(id),
    repository TEXT NOT NULL,
    pr_number INTEGER NOT NULL CHECK (pr_number > 0),
    phase TEXT NOT NULL CHECK (phase IN ('review','fix')),
    round INTEGER NOT NULL,
    attempt INTEGER NOT NULL,
    body TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','published')),
    attempts INTEGER NOT NULL DEFAULT 0,
    last_attempt_at TEXT,
    comment_id INTEGER,
    comment_url TEXT,
    warning_code TEXT NOT NULL DEFAULT '',
    warning TEXT NOT NULL DEFAULT ''
);
CREATE INDEX pending_report_publications ON report_publications(state,run_id);
