-- 0020_job_delivery: how the firmware reaches the device. Empty is the default
-- pull (OtaUrl + Upgrade 1); 'push' is an upload to the device's /u2.
ALTER TABLE ota_jobs ADD COLUMN delivery TEXT NOT NULL DEFAULT '';
