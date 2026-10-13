-- NULL identifies runs that still need advisory diff evaluation.
ALTER TABLE runs ADD COLUMN escalation_json TEXT;
