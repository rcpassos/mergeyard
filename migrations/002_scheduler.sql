-- Component-owned issue context and Git ownership survive control-plane restarts.
CREATE TABLE scheduler_runs (
    run_id TEXT PRIMARY KEY NOT NULL REFERENCES runs(id),
    issue_json TEXT NOT NULL,
    git_json TEXT,
    pr_url TEXT
);
