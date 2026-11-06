-- 0046_snapshot_schedule: the scheduled database snapshot runs on chosen weekdays at a time of day instead
-- of an interval (#602, part 2 of #601). SNAPSHOT_INTERVAL is gone from the settings; a stored interval that
-- is not zero (the schedule was on) becomes every weekday at 03:00 (SNAPSHOT_DAYS, SNAPSHOT_TIME). A stored 0,
-- and no stored value at all, mean off and stay that way (no SNAPSHOT_DAYS). The row is deleted either way.
--
-- snapshot_runs is the record the schedule reads, like backup_runs for the device backups: one row per
-- scheduled attempt ("scheduled": taken or failed, either way that time of day is handled), per skipped
-- time of day ("missed") and per change of the schedule ("rescheduled"). The snapshot files cannot say this:
-- SNAPSHOT_KEEP prunes them, an admin deletes them, and a failed attempt leaves none. The rows follow the
-- audit retention.
--
-- The migration also stores a "rescheduled" marker. The new schedule starts a run that is due and not yet
-- handled; without the marker, the first start after the upgrade within 60 minutes after 03:00 would take
-- that for a missed run and take a snapshot at once. A downgrade restores the .pre-46 copy.
CREATE TABLE snapshot_runs (
    id         INTEGER PRIMARY KEY,
    trigger    TEXT    NOT NULL,
    started_at INTEGER NOT NULL
);
CREATE INDEX snapshot_runs_started ON snapshot_runs(started_at);

INSERT INTO system_settings (key, value, updated_by, updated_at)
SELECT 'SNAPSHOT_DAYS', 'mon,tue,wed,thu,fri,sat,sun', 'migration-0046', CAST(strftime('%s', 'now') AS INTEGER) * 1000
FROM system_settings WHERE key = 'SNAPSHOT_INTERVAL' AND value GLOB '*[1-9]*';

INSERT INTO system_settings (key, value, updated_by, updated_at)
SELECT 'SNAPSHOT_TIME', '03:00', 'migration-0046', CAST(strftime('%s', 'now') AS INTEGER) * 1000
FROM system_settings WHERE key = 'SNAPSHOT_INTERVAL' AND value GLOB '*[1-9]*';

INSERT INTO snapshot_runs (trigger, started_at)
SELECT 'rescheduled', CAST(strftime('%s', 'now') AS INTEGER) * 1000
FROM system_settings WHERE key = 'SNAPSHOT_INTERVAL' AND value GLOB '*[1-9]*';

DELETE FROM system_settings WHERE key = 'SNAPSHOT_INTERVAL';
