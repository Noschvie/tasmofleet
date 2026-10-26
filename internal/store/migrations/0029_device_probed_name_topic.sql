-- 0029_device_probed_name_topic: the DeviceName and the resolved MQTT topic the device reported at
-- the last probe (#91). "Refresh identity" lets the stored name and topic follow the device while
-- they are still these values; a name or topic the user chose differs from them and is kept.
-- Empty: not recorded yet (devices probed before this migration).
ALTER TABLE devices ADD COLUMN device_name TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN device_topic TEXT NOT NULL DEFAULT '';
