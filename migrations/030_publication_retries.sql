-- failed records that the last attempt returned a failure; stopped records that
-- the queue will not retry it again. A finished run stops after a returned
-- failure, while an interrupted attempt or a never-attempted publication still
-- gets one chance. Transient attempts back off from last_attempt_at instead of
-- repeating every tick.
ALTER TABLE report_publications ADD COLUMN failed INTEGER NOT NULL DEFAULT 0 CHECK(failed IN (0, 1));
ALTER TABLE report_publications ADD COLUMN stopped INTEGER NOT NULL DEFAULT 0 CHECK(stopped IN (0, 1));
ALTER TABLE attention_publications ADD COLUMN failed INTEGER NOT NULL DEFAULT 0 CHECK(failed IN (0, 1));
ALTER TABLE attention_publications ADD COLUMN stopped INTEGER NOT NULL DEFAULT 0 CHECK(stopped IN (0, 1));
ALTER TABLE verification_attempts ADD COLUMN stopped INTEGER NOT NULL DEFAULT 0 CHECK(stopped IN (0, 1));

-- An existing warning code usually means the last attempt returned a failure.
-- Keep that result so a terminal run stops instead of retrying once after
-- upgrade; active runs still retry after their backoff. Timeout, cancellation
-- and generic publication failures may be shared-budget expiries, which carry
-- no delivery result, so leave those retryable.
UPDATE report_publications SET failed=1
WHERE warning_code NOT IN ('', 'github.timeout', 'github.canceled', 'publication.failed');
UPDATE attention_publications SET failed=1
WHERE warning_code NOT IN ('', 'github.timeout', 'github.canceled', 'publication.failed');
