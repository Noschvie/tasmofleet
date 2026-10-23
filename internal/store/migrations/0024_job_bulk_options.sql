-- 0024_job_bulk_options: per-transaction options of bulk commands and restarts
-- (design v2, N15). batch_parallel caps how many jobs of the same transaction
-- run at once (0 = the fleet-wide limit applies); reply_timeout_ms is how long a
-- command job waits for each device's answer (0 = the default). Both are fixed
-- when the job is created, so a retry and a resume after a restart keep them.
ALTER TABLE ota_jobs ADD COLUMN batch_parallel INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ota_jobs ADD COLUMN reply_timeout_ms INTEGER NOT NULL DEFAULT 0;
