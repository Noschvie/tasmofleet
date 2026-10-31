-- 0041_device_stability: what TasmoFleet saw of a device's restarts and Wi-Fi signal (#325).
-- device_boots: one row per restart it noticed (MQTT INFO3, or an HTTP poll that saw the uptime drop),
-- capped to the newest 50 per device by the code that writes it. boot_count is Tasmota's persisted
-- BootCount (0 = not known), reason its RestartReason text as sent, uptime_before the seconds the
-- device had been up before it restarted (0 = not known).
CREATE TABLE device_boots (
    id            INTEGER PRIMARY KEY,
    device_id     INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    at            INTEGER NOT NULL,
    boot_count    INTEGER NOT NULL DEFAULT 0,
    reason        TEXT    NOT NULL DEFAULT '',
    uptime_before INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX device_boots_device ON device_boots(device_id, at);

-- device_wifi_samples: the signal in dBm, at most one per device and hour, pruned after 7 days.
CREATE TABLE device_wifi_samples (
    device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    at        INTEGER NOT NULL,
    dbm       INTEGER NOT NULL,
    PRIMARY KEY (device_id, at)
);
