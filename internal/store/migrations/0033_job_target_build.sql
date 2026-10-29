-- 0033_job_target_build: the explicit chip build of an update (ESP32-S2: s2 or s2cdc, #88).
-- Chosen by the user in the update preview and kept on the job, so a retry and a job that
-- resumes after a restart install the same build. Empty: no build was chosen (every other chip,
-- and every job made before this migration).
ALTER TABLE ota_jobs ADD COLUMN target_build TEXT NOT NULL DEFAULT '';
