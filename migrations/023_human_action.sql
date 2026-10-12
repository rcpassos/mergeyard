ALTER TABLE runs ADD COLUMN human_action_json TEXT;

-- Backfill existing faults so attention cards remain actionable after upgrade.
UPDATE runs SET human_action_json = json_object(
 'decision', 'Inspect the saved diagnostics for ' || last_error_code || ': ' || last_error_message || '. Resolve the cause, then run mergeyard retry ' || id || '.',
 'files', json('[]'), 'options', json('[]'))
WHERE state = 'NEEDS_ATTENTION';

-- Frozen attention comments are keyed by the lifecycle event, independent of attempts.
CREATE TABLE attention_publications (
 event_id INTEGER PRIMARY KEY NOT NULL REFERENCES events(id),
 run_id TEXT NOT NULL REFERENCES runs(id),
 repository TEXT NOT NULL,
 issue_number INTEGER NOT NULL CHECK(issue_number > 0),
 body TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','published')),
 attempts INTEGER NOT NULL DEFAULT 0,
 last_attempt_at TEXT,
 comment_id INTEGER,
 comment_url TEXT,
 warning_code TEXT NOT NULL DEFAULT ''
);
