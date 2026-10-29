-- 0034_job_via_safeboot: a push job on an ESP32 with the safeboot layout restarts the device into
-- safeboot and uploads the image there (#30). Decided in the update preview and kept on the job, so
-- a restart of tasmofleet resumes the job in the right mode. 0: no (every job made before this migration).
ALTER TABLE ota_jobs ADD COLUMN via_safeboot INTEGER NOT NULL DEFAULT 0;
-- recovery_step: how far the one recovery of such a job got (0 none, 1 entered, 2 switch back sent),
-- so neither a second recovery nor a second switch back can happen, also across a restart.
ALTER TABLE ota_jobs ADD COLUMN recovery_step INTEGER NOT NULL DEFAULT 0;
