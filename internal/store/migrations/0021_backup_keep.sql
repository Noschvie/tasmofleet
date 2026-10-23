-- 0021_backup_keep: backups an operator pinned ("Keep") are never pruned and
-- do not count against the retention; what started a backup is recorded for
-- the list ("Manual", "Scheduled", "Automatic, before update to 14.3.0").
-- Existing rows have no known trigger (''), which the list shows as before.
ALTER TABLE backups ADD COLUMN kept        INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backups ADD COLUMN trigger     TEXT    NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN trigger_ref TEXT    NOT NULL DEFAULT '';

-- Backups that failed: nothing was stored, so the attempt is kept on its own
-- and shown as a "Failed" row with Retry. reason is a human text without
-- credentials. Pruned with the audit retention.
CREATE TABLE backup_attempts (
    id         INTEGER PRIMARY KEY,
    device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    at         INTEGER NOT NULL,
    kind       TEXT    NOT NULL,
    ref        TEXT    NOT NULL DEFAULT '',
    fw_version TEXT    NOT NULL DEFAULT '',
    reason     TEXT    NOT NULL
);
CREATE INDEX backup_attempts_device ON backup_attempts(device_id, at DESC);
CREATE INDEX backup_attempts_at ON backup_attempts(at);
