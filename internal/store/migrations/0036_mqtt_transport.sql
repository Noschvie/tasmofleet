-- 0036_mqtt_transport: commands, settings, backups, restores and updates can reach a device over
-- MQTT when HTTP fails (#256, #79). What that needs is stored here. No backfill: every old row keeps
-- the value that means "as before".
-- mqtt_password: the device's own MqttPassword, sealed like http_password; only FileDownload and
-- FileUpload need it. mqtt_password_mode: '' not set, 'broker' (use tasmofleet's MQTT_PASSWORD at
-- send time), 'own' (the sealed value). mqtt_password_bad_at: unix ms of the last "Error 1", NULL none.
ALTER TABLE devices ADD COLUMN mqtt_password TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN mqtt_password_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN mqtt_password_bad_at INTEGER;
-- fw_reach: whether the device reaches tasmofleet's firmware server: '' unknown, 'yes', 'no'.
ALTER TABLE devices ADD COLUMN fw_reach TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN fw_reach_at INTEGER;
ALTER TABLE devices ADD COLUMN fw_reach_detail TEXT NOT NULL DEFAULT '';
-- identity_lang_known: 1 once an HTTP probe read the language build; old rows read 0 until the next
-- probe (which runs before every OTA).
ALTER TABLE devices ADD COLUMN identity_lang_known INTEGER NOT NULL DEFAULT 0;
-- transport: how a job reaches its device: '' (every job made before this migration) is HTTP.
-- mqtt_upload: the transaction allowed the experimental firmware upload over MQTT; decided in the
-- preview and kept on the job so a restart resumes it the same way.
ALTER TABLE ota_jobs ADD COLUMN transport TEXT NOT NULL DEFAULT '';
ALTER TABLE ota_jobs ADD COLUMN mqtt_upload INTEGER NOT NULL DEFAULT 0;
ALTER TABLE restore_jobs ADD COLUMN transport TEXT NOT NULL DEFAULT '';
