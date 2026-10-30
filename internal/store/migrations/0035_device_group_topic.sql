-- 0035_device_group_topic: the optional columns OtaUrl and GroupTopic of the device list (#239).
-- group_topic is the plain GroupTopic setting (Status 1 or the INFO1 boot announcement); the
-- two read stamps (unix ms) say when ota_url and group_topic were read. NULL: never read, so a
-- value stored before this migration has no age. Written by the prober / MQTT identity reader only.
ALTER TABLE devices ADD COLUMN group_topic TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN ota_url_read_at INTEGER;
ALTER TABLE devices ADD COLUMN group_topic_read_at INTEGER;
