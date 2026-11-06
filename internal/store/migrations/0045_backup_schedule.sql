-- 0045_backup_schedule: the scheduled device backup runs on chosen weekdays at a time of day instead of an
-- interval (#601). Data only. BACKUP_INTERVAL is gone from the settings; a stored interval that is not zero
-- (the schedule was on) becomes every weekday at 03:00 (BACKUP_DAYS, BACKUP_TIME). A stored 0, and no stored
-- value at all, mean manual only and stay that way (no BACKUP_DAYS). The row is deleted either way.
-- The migration also stores a marker in backup_runs ("rescheduled", completed, started now). The new
-- schedule starts a run that is due and not yet handled; without the marker, the first start after the
-- upgrade within 60 minutes after 03:00 would take that for a missed run and back up the fleet at once,
-- which is the start-up run #612 removes. A downgrade restores the .pre-45 copy.
INSERT INTO system_settings (key, value, updated_by, updated_at)
SELECT 'BACKUP_DAYS', 'mon,tue,wed,thu,fri,sat,sun', 'migration-0045', CAST(strftime('%s', 'now') AS INTEGER) * 1000
FROM system_settings WHERE key = 'BACKUP_INTERVAL' AND value GLOB '*[1-9]*';

INSERT INTO system_settings (key, value, updated_by, updated_at)
SELECT 'BACKUP_TIME', '03:00', 'migration-0045', CAST(strftime('%s', 'now') AS INTEGER) * 1000
FROM system_settings WHERE key = 'BACKUP_INTERVAL' AND value GLOB '*[1-9]*';

INSERT INTO backup_runs (trigger, started_at, finished_at, completed)
SELECT 'rescheduled', CAST(strftime('%s', 'now') AS INTEGER) * 1000, CAST(strftime('%s', 'now') AS INTEGER) * 1000, 1
FROM system_settings WHERE key = 'BACKUP_INTERVAL' AND value GLOB '*[1-9]*';

DELETE FROM system_settings WHERE key = 'BACKUP_INTERVAL';
