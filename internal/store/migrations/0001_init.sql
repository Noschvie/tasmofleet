-- 0001_init: initial schema. See docs/spec "Data model".
-- Timestamps are INTEGER unix milliseconds (UTC). Booleans are INTEGER 0/1.

CREATE TABLE devices (
    id              INTEGER PRIMARY KEY,
    name            TEXT    NOT NULL,
    address         TEXT    NOT NULL,
    port            INTEGER NOT NULL DEFAULT 0,
    mac             TEXT    NOT NULL DEFAULT '',
    topic           TEXT    NOT NULL DEFAULT '',
    full_topic      TEXT    NOT NULL DEFAULT '',
    tags            TEXT    NOT NULL DEFAULT '[]',  -- JSON array of strings
    notes           TEXT    NOT NULL DEFAULT '',
    http_user       TEXT    NOT NULL DEFAULT '',
    http_password   TEXT    NOT NULL DEFAULT '',    -- secretbox sealed, '' = none
    ota_excluded    INTEGER NOT NULL DEFAULT 0,
    mqtt_capability TEXT    NOT NULL DEFAULT '',    -- model.MQTTCapability
    -- identity (fwselect.Detect)
    chip_family     TEXT    NOT NULL DEFAULT '',
    hardware        TEXT    NOT NULL DEFAULT '',
    single_core     INTEGER NOT NULL DEFAULT 0,
    fw_version      TEXT    NOT NULL DEFAULT '',
    fw_image        TEXT    NOT NULL DEFAULT '',
    fw_variant      TEXT    NOT NULL DEFAULT '',
    fw_language     TEXT    NOT NULL DEFAULT '',
    flash_size_kb   INTEGER NOT NULL DEFAULT 0,
    program_flash_kb INTEGER NOT NULL DEFAULT 0,
    free_kb         INTEGER NOT NULL DEFAULT 0,
    identity_at     INTEGER NOT NULL DEFAULT 0,
    -- coarse health, written on change only
    health_state    TEXT    NOT NULL DEFAULT 'unknown',
    last_seen_at    INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX devices_mac   ON devices(mac)   WHERE mac   <> '';
CREATE UNIQUE INDEX devices_topic ON devices(topic) WHERE topic <> '';
CREATE UNIQUE INDEX devices_addr  ON devices(address, port);

CREATE TABLE firmware_artifacts (
    id          INTEGER PRIMARY KEY,
    release     TEXT    NOT NULL,
    filename    TEXT    NOT NULL,
    chip_family TEXT    NOT NULL,
    variant     TEXT    NOT NULL,
    language    TEXT    NOT NULL DEFAULT '',
    safeboot    INTEGER NOT NULL DEFAULT 0,
    size        INTEGER NOT NULL,
    sha256      TEXT    NOT NULL,
    source      TEXT    NOT NULL,
    source_url  TEXT    NOT NULL,
    fetched_at  INTEGER NOT NULL,
    UNIQUE (release, filename)
);

CREATE TABLE ota_batches (
    id          INTEGER PRIMARY KEY,
    created_by  TEXT    NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE TABLE ota_jobs (
    id               INTEGER PRIMARY KEY,
    batch_id         INTEGER NOT NULL REFERENCES ota_batches(id),
    device_id        INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    state            TEXT    NOT NULL,
    stage            TEXT    NOT NULL,
    plan             TEXT    NOT NULL DEFAULT '',
    policy           TEXT    NOT NULL DEFAULT 'only-newer',
    skip_backup      INTEGER NOT NULL DEFAULT 0,
    attempt          INTEGER NOT NULL DEFAULT 0,
    target_release   TEXT    NOT NULL,
    target_variant   TEXT    NOT NULL,
    target_artifact  INTEGER NOT NULL DEFAULT 0,
    minimal_artifact INTEGER NOT NULL DEFAULT 0,
    identity_before  TEXT    NOT NULL DEFAULT '{}', -- JSON model.Identity
    original_ota_url TEXT    NOT NULL DEFAULT '',
    uptime_before    INTEGER NOT NULL DEFAULT 0,
    seen_offline     INTEGER NOT NULL DEFAULT 0,
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    auto_continue_seen INTEGER NOT NULL DEFAULT 0,
    deadline_at      INTEGER NOT NULL DEFAULT 0,
    last_error       TEXT    NOT NULL DEFAULT '',
    created_by       TEXT    NOT NULL,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
-- At most one non-terminal job per device.
CREATE UNIQUE INDEX ota_jobs_one_active ON ota_jobs(device_id)
    WHERE state NOT IN ('succeeded', 'failed', 'cancelled');
CREATE INDEX ota_jobs_batch ON ota_jobs(batch_id);

CREATE TABLE ota_job_events (
    id      INTEGER PRIMARY KEY,
    job_id  INTEGER NOT NULL REFERENCES ota_jobs(id) ON DELETE CASCADE,
    at      INTEGER NOT NULL,
    from_state TEXT NOT NULL,
    to_state   TEXT NOT NULL,
    event   TEXT    NOT NULL,
    detail  TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX ota_job_events_job ON ota_job_events(job_id, id);

CREATE TABLE backups (
    id         INTEGER PRIMARY KEY,
    device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL,
    taken_at   INTEGER NOT NULL,
    fw_version TEXT    NOT NULL DEFAULT '',
    path       TEXT    NOT NULL,
    size       INTEGER NOT NULL,
    sha256     TEXT    NOT NULL,
    decoder_version TEXT NOT NULL DEFAULT '',
    note       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX backups_device ON backups(device_id, taken_at DESC);

-- Health state transitions (not every observation), pruned by retention.
CREATE TABLE health_events (
    id         INTEGER PRIMARY KEY,
    device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    at         INTEGER NOT NULL,
    from_state TEXT    NOT NULL,
    to_state   TEXT    NOT NULL,
    source     TEXT    NOT NULL,
    detail     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX health_events_device ON health_events(device_id, at);

CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY,
    at          INTEGER NOT NULL,
    actor       TEXT    NOT NULL,
    action      TEXT    NOT NULL,
    target      TEXT    NOT NULL DEFAULT '',
    detail      TEXT    NOT NULL DEFAULT '{}',
    result      TEXT    NOT NULL,
    remote_addr TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_at ON audit_log(at);

CREATE TABLE sessions (
    id_hash     TEXT    PRIMARY KEY,
    principal   TEXT    NOT NULL, -- JSON model.Principal
    csrf_token  TEXT    NOT NULL,
    id_token    TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
);

CREATE TABLE local_users (
    username      TEXT PRIMARY KEY,
    password_hash TEXT    NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
