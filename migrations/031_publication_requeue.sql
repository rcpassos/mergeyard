-- A stopped publication keeps the reason it stopped, so an operator can see
-- why it will not be retried before asking for another attempt. A requeue
-- clears the stop and the reason together.
ALTER TABLE attention_publications ADD COLUMN warning TEXT NOT NULL DEFAULT '';

-- The body publication keeps its reason like the other publications, and its
-- attempt counter may start over after a requeue, so its next failure waits
-- the first backoff again. Rebuild the row with both columns.
CREATE TABLE verification_attempts_requeued (
    run_id TEXT PRIMARY KEY NOT NULL REFERENCES runs(id),
    source_attempt TEXT NOT NULL REFERENCES phase_attempts(id),
    attempts INTEGER NOT NULL DEFAULT 1 CHECK (attempts >= 0),
    last_attempt_at TEXT NOT NULL,
    failed INTEGER NOT NULL DEFAULT 0 CHECK (failed IN (0, 1)),
    stopped INTEGER NOT NULL DEFAULT 0 CHECK (stopped IN (0, 1)),
    warning_code TEXT NOT NULL DEFAULT '',
    warning TEXT NOT NULL DEFAULT ''
);
INSERT INTO verification_attempts_requeued(run_id,source_attempt,attempts,last_attempt_at,failed,stopped)
SELECT run_id,source_attempt,attempts,last_attempt_at,failed,stopped FROM verification_attempts;
DROP TABLE verification_attempts;
ALTER TABLE verification_attempts_requeued RENAME TO verification_attempts;

-- Recover the reason of the publications that already stopped from the
-- append-only event history. A first permanent failure records a warning
-- event; a repeated one records the stop. The newest matching event carries
-- the reason the publication stopped, and the newest event that names a code
-- carries the code.
UPDATE attention_publications SET
  warning_code = COALESCE((
    SELECT json_extract(e.payload_json, '$.code') FROM events e
    WHERE e.run_id = attention_publications.run_id
      AND e.type IN ('publication.warning', 'publication.stopped')
      AND json_extract(e.payload_json, '$.attention_event_id') = attention_publications.event_id
      AND json_extract(e.payload_json, '$.code') IS NOT NULL
    ORDER BY e.id DESC LIMIT 1), warning_code),
  warning = COALESCE((
    SELECT json_extract(e.payload_json, '$.message') FROM events e
    WHERE e.run_id = attention_publications.run_id
      AND e.type IN ('publication.warning', 'publication.stopped')
      AND json_extract(e.payload_json, '$.attention_event_id') = attention_publications.event_id
    ORDER BY e.id DESC LIMIT 1), warning);
UPDATE verification_attempts SET
  warning_code = COALESCE((
    SELECT json_extract(e.payload_json, '$.code') FROM events e
    WHERE e.run_id = verification_attempts.run_id
      AND e.type IN ('verification.warning', 'publication.stopped')
      AND json_extract(e.payload_json, '$.source_attempt') = verification_attempts.source_attempt
      AND json_extract(e.payload_json, '$.code') IS NOT NULL
    ORDER BY e.id DESC LIMIT 1), ''),
  warning = COALESCE((
    SELECT json_extract(e.payload_json, '$.message') FROM events e
    WHERE e.run_id = verification_attempts.run_id
      AND e.type IN ('verification.warning', 'publication.stopped')
      AND json_extract(e.payload_json, '$.source_attempt') = verification_attempts.source_attempt
    ORDER BY e.id DESC LIMIT 1), '');
