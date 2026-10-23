-- 0009_system_settings: operational settings edited in the web UI (system
-- settings page) and the versioned fleet policy rules. A stored setting only
-- applies when the operator did not set the same key in the environment.
CREATE TABLE system_settings (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    sealed     INTEGER NOT NULL DEFAULT 0, -- 1: value is encrypted with the secretbox (secrets)
    target     TEXT    NOT NULL DEFAULT '', -- secrets: the scheme://host:port the secret was entered for; it is only used for that target
    updated_by TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Every save of the managed policy rules is a new row; the newest is current.
-- A revert is a new row too (revert_of names the restored version).
CREATE TABLE policy_rule_versions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    rules      TEXT    NOT NULL, -- JSON array in the POLICY_RULES format
    note       TEXT    NOT NULL DEFAULT '',
    actor      TEXT    NOT NULL,
    revert_of  INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);
