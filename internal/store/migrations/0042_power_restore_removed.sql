-- 0042_power_restore_removed: TasmoFleet no longer switches relays back on after an update (#551).
-- Data only. A job that was waiting in restore_power had already flashed the final firmware, so it
-- ends as succeeded (the update itself had worked); the restore step it waited for no longer exists
-- and a non-terminal job would also block its device through ota_jobs_one_active. The stored
-- OTA_POWER_MAX_AGE setting has no meaning any more. The power columns of ota_jobs (0007, 0008) stay unused;
-- dropping them would rebuild the table for nothing. A downgrade restores the .pre-42 copy.
INSERT INTO ota_job_events (job_id, at, from_state, to_state, event, detail)
SELECT id, updated_at, 'restore_power', 'succeeded', 'migration',
       '0042: the power restore was removed in 0.28.0; the update itself had succeeded'
FROM ota_jobs WHERE state = 'restore_power';

UPDATE ota_jobs SET state = 'succeeded' WHERE state = 'restore_power';

DELETE FROM system_settings WHERE key = 'OTA_POWER_MAX_AGE';
