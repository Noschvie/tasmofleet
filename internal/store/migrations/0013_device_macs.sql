-- 0013_device_macs: a device is identified by a SET of MACs (spec 11.1): the
-- Wi-Fi station MAC plus, where present, the Ethernet MAC. devices.mac stays
-- the primary MAC; this table holds the whole set, primary included. A MAC
-- belongs to at most one device. The set is seeded from the existing column.
--
-- Credentials are not migrated: all stored devices that came from a
-- TasmoAdmin import have http_user set and no password. The load path treats
-- "no password" as "no credentials" and ignores such a user (Tasmota's web
-- user is always "admin"); nothing is deleted here.
CREATE TABLE device_macs (
    mac       TEXT    NOT NULL PRIMARY KEY,
    device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE
);
CREATE INDEX device_macs_device ON device_macs(device_id);
INSERT INTO device_macs (mac, device_id) SELECT mac, id FROM devices WHERE mac <> '';
