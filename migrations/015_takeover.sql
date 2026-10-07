-- Intent precedes interruption/restoration. Attention requires an explicit retry
-- of takeover; manual control is never automatically handed back.
ALTER TABLE runs ADD COLUMN takeover_status TEXT NOT NULL DEFAULT ''
    CHECK (takeover_status IN ('', 'requested', 'manual', 'attention', 'cancelled'));
