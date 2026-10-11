-- Pin implementation publication and retain check-retry provenance.
ALTER TABLE phase_attempts ADD COLUMN commit_sha TEXT;
ALTER TABLE phase_attempts ADD COLUMN check_retry_of TEXT NOT NULL DEFAULT '';
-- Agent identities and system command attempts have separate number sequences.
DROP INDEX fix_attempt_identity;
CREATE UNIQUE INDEX fix_attempt_identity ON phase_attempts(run_id,phase,round,attempt) WHERE phase='fix' AND step='';
DROP INDEX review_attempt_identity;
CREATE UNIQUE INDEX review_attempt_identity ON phase_attempts(run_id,phase,round,attempt) WHERE phase='review' AND step='';
