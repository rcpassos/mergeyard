-- Stop intent survives remote cleanup failures and control-plane restarts.
ALTER TABLE runs ADD COLUMN stop_requested INTEGER NOT NULL DEFAULT 0 CHECK (stop_requested IN (0, 1));

-- NULL means an older attempt did not record its skills; [] means none used.
ALTER TABLE phase_attempts ADD COLUMN skills_json TEXT;
