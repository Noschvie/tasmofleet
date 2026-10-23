-- 0008_power_restore_relays: lights only by default (relays on request) and
-- the moment the device first went offline, which separates a user's MQTT
-- "switched off" from the device's boot announcements.
ALTER TABLE ota_jobs ADD COLUMN restore_relays INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ota_jobs ADD COLUMN power_cutoff INTEGER NOT NULL DEFAULT 0;
