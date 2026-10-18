-- The implement report is attempt evidence, like review and fix reports. Keep
-- it in typed columns beside the attempt so readers never reconstruct it from
-- completion-event payloads. report_eligible freezes the publication rule at
-- the completion that saved the report: later lifecycle events rewrite the
-- attempt's error, but they must never withdraw an already-saved report.
ALTER TABLE phase_attempts ADD COLUMN report_json TEXT;
ALTER TABLE phase_attempts ADD COLUMN report_eligible INTEGER NOT NULL DEFAULT 0 CHECK (report_eligible IN (0,1));

-- Backfill reports saved by older binaries from their completion events, using
-- the rule the dashboard always used: the newest result, whatever the
-- completion's error. Legacy implement events omit the phase; review and fix
-- events always record theirs. A zero-version result is not a report.
UPDATE phase_attempts SET report_json=(
    SELECT json_extract(e.payload_json,'$.result') FROM events e
    WHERE e.run_id=phase_attempts.run_id
      AND e.type IN ('phase.completed','phase.failed')
      AND COALESCE(json_extract(e.payload_json,'$.phase'),'implement')='implement'
      AND json_extract(e.payload_json,'$.attempt')=phase_attempts.attempt
      AND json_type(e.payload_json,'$.result')='object'
      AND COALESCE(json_extract(e.payload_json,'$.result.schema_version'),0)>0
    ORDER BY e.id DESC LIMIT 1)
WHERE phase='implement' AND step='';

-- Freeze eligibility exactly as the event-era reader derived it: an attempt was
-- publishable when one of its completions was a normal success or a valid
-- blocked/failed result.
UPDATE phase_attempts SET report_eligible=1
WHERE phase='implement' AND step='' AND report_json IS NOT NULL AND EXISTS (
    SELECT 1 FROM events e
    WHERE e.run_id=phase_attempts.run_id
      AND e.type IN ('phase.completed','phase.failed')
      AND COALESCE(json_extract(e.payload_json,'$.phase'),'implement')='implement'
      AND json_extract(e.payload_json,'$.attempt')=phase_attempts.attempt
      AND json_type(e.payload_json,'$.result')='object'
      AND COALESCE(json_extract(e.payload_json,'$.result.schema_version'),0)>0
      AND (COALESCE(json_extract(e.payload_json,'$.error'),'')='' OR json_extract(e.payload_json,'$.error') LIKE 'phase.blocked:%' OR json_extract(e.payload_json,'$.error') LIKE 'phase.failed:%'));

-- Fix reports were stored for any completion; only an eligible completion makes
-- them publishable, exactly as the event-era reader derived it.
UPDATE phase_attempts SET report_eligible=1
WHERE phase='fix' AND step='' AND EXISTS (
    SELECT 1 FROM fix_attempts f JOIN events e
        ON e.run_id=phase_attempts.run_id
        AND e.type IN ('phase.completed','phase.failed')
        AND COALESCE(json_extract(e.payload_json,'$.phase'),'implement')='fix'
        AND json_extract(e.payload_json,'$.round')=phase_attempts.round
        AND json_extract(e.payload_json,'$.attempt')=phase_attempts.attempt
        AND json_type(e.payload_json,'$.result')='object'
        AND COALESCE(json_extract(e.payload_json,'$.result.schema_version'),0)>0
        AND (COALESCE(json_extract(e.payload_json,'$.error'),'')='' OR json_extract(e.payload_json,'$.error') LIKE 'phase.blocked:%' OR json_extract(e.payload_json,'$.error') LIKE 'phase.failed:%')
    WHERE f.attempt_id=phase_attempts.id AND f.report_json IS NOT NULL);

-- Publication bookkeeping referenced completion events; the reader now answers
-- attempts. Rebuild the bounded per-run row against the attempt that carries
-- the report. A row whose completion event no longer resolves is dropped; the
-- next tick re-attempts the run and records it again.
CREATE TABLE verification_attempts_upgraded (
    run_id TEXT PRIMARY KEY NOT NULL REFERENCES runs(id),
    source_attempt TEXT NOT NULL REFERENCES phase_attempts(id),
    attempts INTEGER NOT NULL DEFAULT 1 CHECK (attempts > 0),
    last_attempt_at TEXT NOT NULL,
    failed INTEGER NOT NULL DEFAULT 0 CHECK (failed IN (0, 1))
);
WITH mapped(run_id,source_attempt,attempts,last_attempt_at,failed) AS (
    SELECT v.run_id,(
        SELECT a.id FROM events src JOIN phase_attempts a
            ON a.run_id=src.run_id
            AND a.step=''
            AND a.phase=COALESCE(json_extract(src.payload_json,'$.phase'),'implement')
            AND a.phase IN ('implement','fix')
            AND a.attempt=json_extract(src.payload_json,'$.attempt')
            AND (a.phase<>'fix' OR a.round=json_extract(src.payload_json,'$.round'))
        WHERE src.id=v.source_event
            AND src.type IN ('phase.completed','phase.failed')
        ORDER BY a.rowid DESC LIMIT 1),v.attempts,v.last_attempt_at,v.failed
    FROM verification_attempts v
)
INSERT INTO verification_attempts_upgraded(run_id,source_attempt,attempts,last_attempt_at,failed)
SELECT run_id,source_attempt,attempts,last_attempt_at,failed FROM mapped WHERE source_attempt IS NOT NULL;
DROP TABLE verification_attempts;
ALTER TABLE verification_attempts_upgraded RENAME TO verification_attempts;

-- Receipts and warnings referenced completion event IDs; the reader now answers
-- attempts. Point existing ones at the attempt that carries the report so
-- already-published bodies are not published again on upgrade. A reference that
-- no longer resolves keeps its old key and republishes once; the event stream is
-- append-only, so only corrupted history reaches that path.
WITH rekey(event_id,attempt_id) AS (
    SELECT p.id,(
        SELECT a.id FROM events src JOIN phase_attempts a
            ON a.run_id=src.run_id
            AND a.step=''
            AND a.phase=COALESCE(json_extract(src.payload_json,'$.phase'),'implement')
            AND a.phase IN ('implement','fix')
            AND a.attempt=json_extract(src.payload_json,'$.attempt')
            AND (a.phase<>'fix' OR a.round=json_extract(src.payload_json,'$.round'))
        WHERE src.id=json_extract(p.payload_json,'$.source_event')
            AND src.type IN ('phase.completed','phase.failed')
        ORDER BY a.rowid DESC LIMIT 1)
    FROM events p
    WHERE p.type IN ('verification.published','verification.warning')
      AND json_extract(p.payload_json,'$.source_event') IS NOT NULL)
UPDATE events SET payload_json=json_set(payload_json,'$.source_attempt',(
    SELECT attempt_id FROM rekey WHERE event_id=events.id))
WHERE id IN (SELECT event_id FROM rekey WHERE attempt_id IS NOT NULL);
