-- NULL distinguishes attempts created before permission settings were recorded.
ALTER TABLE phase_attempts ADD COLUMN permissions TEXT;
