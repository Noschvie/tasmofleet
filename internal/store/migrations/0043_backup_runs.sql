-- 0043_backup_runs: the record of the scheduled backup runs (#409), so the status bar, the dashboard
-- and the Devices, Backups page show the last run after a restart.
-- backup_runs: one row per scheduled run. tried = ok + failed; skipped devices (offline, never
-- answered, busy) were not tried and are counted apart. completed is 0 for a run that shutdown cut short.
-- backup_run_items: one row per device of a run. device_id is NULL once the device is deleted (the name stays).
-- Both follow the audit retention.
CREATE TABLE backup_runs (
    id          INTEGER PRIMARY KEY,
    trigger     TEXT    NOT NULL,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0,
    completed   INTEGER NOT NULL DEFAULT 0,
    tried       INTEGER NOT NULL DEFAULT 0,
    ok          INTEGER NOT NULL DEFAULT 0,
    failed      INTEGER NOT NULL DEFAULT 0,
    skipped     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX backup_runs_started ON backup_runs(started_at);

CREATE TABLE backup_run_items (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER NOT NULL REFERENCES backup_runs(id) ON DELETE CASCADE,
    device_id   INTEGER REFERENCES devices(id) ON DELETE SET NULL,
    device_name TEXT    NOT NULL,
    outcome     TEXT    NOT NULL,
    reason      TEXT    NOT NULL DEFAULT '',
    at          INTEGER NOT NULL
);
CREATE INDEX backup_run_items_run ON backup_run_items(run_id);
CREATE INDEX backup_run_items_device ON backup_run_items(device_id);
