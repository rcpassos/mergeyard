-- NULL preserves configuration-based behavior for runs claimed before this upgrade.
ALTER TABLE runs ADD COLUMN role_settings_json TEXT;

-- Invalid overrides create queue diagnostics, never executable runs. Journal
-- pending comment/label delivery so interrupted writes can be retried safely.
CREATE TABLE harness_override_refusals (
 repository TEXT NOT NULL,
 issue_number INTEGER NOT NULL,
 reason TEXT NOT NULL,
 attention_label TEXT NOT NULL,
 pending INTEGER NOT NULL DEFAULT 1,
 PRIMARY KEY (repository, issue_number)
);
