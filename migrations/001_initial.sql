CREATE TABLE runs (
    id TEXT PRIMARY KEY NOT NULL,
    repository TEXT NOT NULL,
    issue_number INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN (
        'CLAIMING', 'PREPARING', 'ACTIVE', 'WAITING_FOR_CI',
        'WAITING_FOR_HARNESS', 'READY_TO_MERGE', 'MANUAL',
        'NEEDS_ATTENTION', 'FAILED', 'STOPPED', 'COMPLETED'
    )),
    current_phase TEXT,
    review_round INTEGER NOT NULL DEFAULT 0,
    branch TEXT,
    worktree_path TEXT,
    pr_number INTEGER,
    implementer_agent TEXT,
    implementer_session_id TEXT,
    reviewer_agent TEXT,
    reviewer_session_id TEXT,
    approved_sha TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    completed_at TEXT,
    last_error_code TEXT,
    last_error_message TEXT
);

CREATE UNIQUE INDEX runs_one_non_terminal_per_issue
    ON runs(repository, issue_number)
    WHERE state NOT IN ('FAILED', 'STOPPED', 'COMPLETED');

CREATE TABLE phase_attempts (
    id TEXT PRIMARY KEY NOT NULL,
    run_id TEXT NOT NULL REFERENCES runs(id),
    phase TEXT NOT NULL,
    role TEXT NOT NULL,
    round INTEGER NOT NULL,
    attempt INTEGER NOT NULL,
    agent TEXT NOT NULL,
    model TEXT,
    effort TEXT,
    status TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'usage_limited', 'stopped')),
    resumed_session INTEGER NOT NULL DEFAULT 0 CHECK (resumed_session IN (0, 1)),
    process_session TEXT,
    input_path TEXT,
    result_path TEXT,
    log_path TEXT,
    exit_code INTEGER,
    started_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ended_at TEXT,
    error TEXT
);

CREATE TABLE harness_limits (
    harness_type TEXT PRIMARY KEY NOT NULL,
    limited_until TEXT NOT NULL,
    reset_time_source TEXT NOT NULL CHECK (reset_time_source IN ('reported', 'default_cooldown')),
    detected_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    phase_attempt_id TEXT REFERENCES phase_attempts(id)
);

CREATE TABLE events (
    id INTEGER PRIMARY KEY,
    run_id TEXT REFERENCES runs(id),
    type TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
