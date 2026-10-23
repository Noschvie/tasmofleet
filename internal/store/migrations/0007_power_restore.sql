-- 0005_power_restore: relay state read before an OTA update and restored
-- after it (the two-step ESP8266 path loses the RTC-RAM power state).
ALTER TABLE ota_jobs ADD COLUMN skip_power_restore INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ota_jobs ADD COLUMN power_on INTEGER NOT NULL DEFAULT 0; -- bit n-1 set: relay n was ON before the update
ALTER TABLE ota_jobs ADD COLUMN power_read_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ota_jobs ADD COLUMN power_note TEXT NOT NULL DEFAULT '';
