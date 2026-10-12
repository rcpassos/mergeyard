-- Refusals are queue diagnostics, never executable runs. Pending side effects
-- survive restart until the marker comment and ready-label removal succeed.
CREATE TABLE issue_refusals (
 repository TEXT NOT NULL,
 issue_number INTEGER NOT NULL,
 reason TEXT NOT NULL,
 ready_label TEXT NOT NULL,
 pending INTEGER NOT NULL DEFAULT 1,
 PRIMARY KEY (repository, issue_number)
);
