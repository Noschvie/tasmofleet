-- 0053_device_mqtt_password_source: where a device's MqttPassword came from, and since when (#609).
--
-- mqtt_password_source: '' typed in the edit form, 'backup' taken from the device's newest decoded backup.
-- mqtt_password_set_at: unix ms of the fact: the save, or the TakenAt of the backup; NULL never set. A backup
-- replaces the stored password only when it is newer than this ("the newer fact wins").
-- mqtt_password_ok_at: unix ms of the last password test the device accepted; NULL not tested since the last
-- change, or refused since.
ALTER TABLE devices ADD COLUMN mqtt_password_source TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN mqtt_password_set_at INTEGER;
ALTER TABLE devices ADD COLUMN mqtt_password_ok_at INTEGER;
-- A password typed before this migration has no date. updated_at is the latest time it can have been typed (every
-- save bumps it), so an older backup does not replace it; the next backup after the upgrade settles it. Devices
-- without a password keep NULL, so the first pass over the existing backups fills them.
UPDATE devices SET mqtt_password_set_at = updated_at WHERE mqtt_password_mode != '';
