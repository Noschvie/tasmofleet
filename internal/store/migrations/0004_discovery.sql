-- 0004_discovery: proposals from MQTT discovery and the HTTP autoscan (M2-3).
-- Everything in this table comes from the network and is untrusted until an
-- admin accepts it (see docs/security/discovery.md). One row per MAC.
CREATE TABLE discovery_proposals (
    id          INTEGER PRIMARY KEY,
    mac         TEXT    NOT NULL UNIQUE,
    kind        TEXT    NOT NULL CHECK (kind IN ('new','address-change','conflict')),
    via         TEXT    NOT NULL CHECK (via IN ('mqtt','scan')),
    reason      TEXT    NOT NULL DEFAULT '',
    name        TEXT    NOT NULL DEFAULT '',
    address     TEXT    NOT NULL,
    port        INTEGER NOT NULL DEFAULT 0,
    topic       TEXT    NOT NULL DEFAULT '',
    full_topic  TEXT    NOT NULL DEFAULT '',
    existing_id INTEGER NOT NULL DEFAULT 0,  -- inventory device the proposal refers to, 0 = none
    dismissed   INTEGER NOT NULL DEFAULT 0,  -- hidden, but kept so a retained message does not bring it back
    first_seen  INTEGER NOT NULL,
    seen_at     INTEGER NOT NULL
);
CREATE INDEX discovery_proposals_seen ON discovery_proposals(seen_at);
