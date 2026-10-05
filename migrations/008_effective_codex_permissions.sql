-- Full access has network permission regardless of the workspace-write toggle.
-- Correct snapshots recorded before effective permissions were displayed.
UPDATE phase_attempts
SET permissions = 'danger-full-access · network true · approvals never'
WHERE agent = 'codex'
  AND permissions = 'danger-full-access · network false · approvals never';
