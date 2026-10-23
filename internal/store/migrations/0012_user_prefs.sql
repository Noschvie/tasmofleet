-- Per-user interface preferences (table columns, page size), keyed on the
-- stable principal subject so they follow the user across devices and sign-in
-- modes. The value is a small JSON document validated by the web layer.
CREATE TABLE user_prefs (
    user_key   TEXT    NOT NULL,
    pref_key   TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_key, pref_key)
) WITHOUT ROWID;
