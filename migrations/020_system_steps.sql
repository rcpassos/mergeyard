-- System steps remain attempts of the owning phase, without spending its agent budget.
ALTER TABLE phase_attempts ADD COLUMN step TEXT NOT NULL DEFAULT '';
ALTER TABLE phase_attempts ADD COLUMN system_step_json TEXT;
