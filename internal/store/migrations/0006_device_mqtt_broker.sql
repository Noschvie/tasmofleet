-- The broker a device is configured for (Status 6), shown in the UI and
-- compared with the tasmofleet broker. Host and port only, never credentials.
ALTER TABLE devices ADD COLUMN mqtt_host TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN mqtt_port INTEGER NOT NULL DEFAULT 0;
