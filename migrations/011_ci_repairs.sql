-- Retain the exact CI repair context after a new review replaces the CI wait.
ALTER TABLE fix_attempts ADD COLUMN ci_json TEXT;
