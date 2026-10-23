-- Principals: the display name of everyone who signed in, keyed on the stable
-- subject (an OIDC sub, a proxy header user, "local:<name>"). Audit rows,
-- jobs and backups keep storing the subject; the UI shows the name captured
-- at the last sign-in. kind is "sso", "local" or "header".
CREATE TABLE principals (
    subject      TEXT    NOT NULL PRIMARY KEY,
    display_name TEXT    NOT NULL,
    kind         TEXT    NOT NULL,
    updated_at   INTEGER NOT NULL
) WITHOUT ROWID;
