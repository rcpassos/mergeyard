-- A stop survives interruption or failed label writes and is retried before work.
ALTER TABLE runs ADD COLUMN stop_requested INTEGER NOT NULL DEFAULT 0 CHECK (stop_requested IN (0, 1));
