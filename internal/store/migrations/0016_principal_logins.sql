-- Users page: what the page shows about someone who signs in through the
-- identity provider or a trusted proxy. role is the role the last sign-in
-- resolved to (it comes from the IdP groups, so it is only a record of the
-- last login); last_login_at is that sign-in in epoch milliseconds, 0 when the
-- row predates this column.
ALTER TABLE principals ADD COLUMN role          TEXT    NOT NULL DEFAULT '';
ALTER TABLE principals ADD COLUMN last_login_at INTEGER NOT NULL DEFAULT 0;
