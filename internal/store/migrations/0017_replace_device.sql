-- 0017_replace_device: a restore job can also be a "replace device" run, where
-- the stored backup of a broken device is restored onto new hardware and the
-- inventory entry then takes over the new hardware's MAC set. The job keeps
-- what is needed to show and audit it: which hardware it targets and where
-- that hardware was found. Existing jobs are plain restores.
ALTER TABLE restore_jobs ADD COLUMN kind        TEXT    NOT NULL DEFAULT 'restore';
ALTER TABLE restore_jobs ADD COLUMN new_mac     TEXT    NOT NULL DEFAULT '';
ALTER TABLE restore_jobs ADD COLUMN new_address TEXT    NOT NULL DEFAULT '';
ALTER TABLE restore_jobs ADD COLUMN new_port    INTEGER NOT NULL DEFAULT 0;
