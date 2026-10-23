-- 0022_backup_orphans: a device can be deleted while its backups are kept.
-- backups.device_id becomes nullable with ON DELETE SET NULL (it cascaded),
-- and every backup carries the device name and MAC it was taken for, so a
-- kept backup of a deleted device still says whose it is. SQLite cannot change
-- a column's constraint in place, so the table is rebuilt and the rows are
-- copied; name and MAC of existing rows come from their device. No table
-- references backups (restore_jobs.backup_id has no foreign key), so the drop
-- and rename touch nothing else.
CREATE TABLE backups_new (
    id              INTEGER PRIMARY KEY,
    device_id       INTEGER REFERENCES devices(id) ON DELETE SET NULL,
    device_name     TEXT    NOT NULL DEFAULT '',
    device_mac      TEXT    NOT NULL DEFAULT '',
    kind            TEXT    NOT NULL,
    taken_at        INTEGER NOT NULL,
    fw_version      TEXT    NOT NULL DEFAULT '',
    path            TEXT    NOT NULL,
    size            INTEGER NOT NULL,
    sha256          TEXT    NOT NULL,
    decoder_version TEXT    NOT NULL DEFAULT '',
    note            TEXT    NOT NULL DEFAULT '',
    kept            INTEGER NOT NULL DEFAULT 0,
    trigger         TEXT    NOT NULL DEFAULT '',
    trigger_ref     TEXT    NOT NULL DEFAULT ''
);
INSERT INTO backups_new (id, device_id, device_name, device_mac, kind, taken_at, fw_version, path, size,
                         sha256, decoder_version, note, kept, trigger, trigger_ref)
SELECT b.id, b.device_id, COALESCE(d.name, ''), COALESCE(d.mac, ''), b.kind, b.taken_at, b.fw_version, b.path, b.size,
       b.sha256, b.decoder_version, b.note, b.kept, b.trigger, b.trigger_ref
FROM backups b LEFT JOIN devices d ON d.id = b.device_id;
DROP TABLE backups;
ALTER TABLE backups_new RENAME TO backups;
CREATE INDEX backups_device ON backups(device_id, taken_at DESC);
