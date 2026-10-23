-- 0009_batch_retry: a retry of failed devices is a new batch (transaction)
-- that points at the one it retries; one batch can have several retries.
ALTER TABLE ota_batches ADD COLUMN retry_of INTEGER NOT NULL DEFAULT 0; -- 0 = not a retry
CREATE INDEX ota_batches_retry_of ON ota_batches(retry_of) WHERE retry_of <> 0;
