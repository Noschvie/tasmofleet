-- Config restore jobs (M2-5). One row per restore of a stored backup onto its
-- device. The one-time download token is never stored: it lives in memory
-- only, so a restart invalidates every open token.
CREATE TABLE restore_jobs (
    id            INTEGER PRIMARY KEY,
    device_id     INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    backup_id     INTEGER NOT NULL,                -- the dump that is restored (no FK: retention may delete it)
    pre_backup_id INTEGER NOT NULL DEFAULT 0,      -- automatic backup taken just before
    state         TEXT    NOT NULL,                -- pending, backup, sending, waiting, verifying, done, failed
    detail        TEXT    NOT NULL DEFAULT '',     -- human-readable result or error, no secrets
    verified      INTEGER NOT NULL DEFAULT 0,      -- 1 = verification diff was empty (volatile fields ignored)
    changes       INTEGER NOT NULL DEFAULT 0,      -- rows left in the verification diff
    actor         TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX restore_jobs_device ON restore_jobs(device_id, created_at DESC);
-- At most one running restore per device.
CREATE UNIQUE INDEX restore_jobs_active ON restore_jobs(device_id) WHERE state NOT IN ('done','failed');
