-- Event reads are always scoped to one run and one lifecycle meaning. Without
-- this index every dashboard timeline, implement snapshot and verification
-- publication pass scans the whole history to find one run's rows.
CREATE INDEX events_run_type ON events(run_id, type);

-- Reconciliation and observation resolve a run's attempts by phase and round,
-- including for runs that finished long ago. The partial unique indexes added
-- in 021 only answer review and fix identity lookups, so implement attempts and
-- step-agnostic reads need this index instead.
CREATE INDEX phase_attempts_run_phase ON phase_attempts(run_id, phase, round, attempt);

-- Only an in-flight attempt can yield a discovered session identity, so the
-- 250ms observation pass stays proportional to live work.
CREATE INDEX running_phase_attempts ON phase_attempts(run_id, phase, round) WHERE status='running';
