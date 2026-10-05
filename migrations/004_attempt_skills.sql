-- NULL means an older attempt did not record its skills; [] means none used.
ALTER TABLE phase_attempts ADD COLUMN skills_json TEXT;
