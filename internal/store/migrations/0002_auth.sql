-- Local accounts with roles, sealed OIDC refresh tokens, per-cookie device
-- cookie revocation and password generations (WP6b).

-- Existing accounts are the former break-glass admins: they keep admin rights.
ALTER TABLE local_users ADD COLUMN role TEXT NOT NULL DEFAULT 'admin' CHECK (role IN ('operator','admin'));
ALTER TABLE local_users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE local_users ADD COLUMN must_change_password INTEGER NOT NULL DEFAULT 0;
-- Incremented on every password change and on disable. Sessions and device
-- cookies carry the generation their login verified and are valid only while
-- it is current, so a login that raced a change dies with it.
ALTER TABLE local_users ADD COLUMN password_gen INTEGER NOT NULL DEFAULT 1;

ALTER TABLE sessions ADD COLUMN subject TEXT NOT NULL DEFAULT '';        -- Principal.Subject, for revoking all sessions of a user
ALTER TABLE sessions ADD COLUMN refresh_token TEXT NOT NULL DEFAULT '';  -- OIDC refresh token, sealed with secretbox
ALTER TABLE sessions ADD COLUMN rechecked_at INTEGER NOT NULL DEFAULT 0; -- last OIDC group re-check
ALTER TABLE sessions ADD COLUMN password_gen INTEGER NOT NULL DEFAULT 0; -- local_users.password_gen the login verified
UPDATE sessions SET subject = COALESCE(json_extract(principal, '$.Subject'), '');
CREATE INDEX sessions_subject ON sessions(subject);

-- One row per issued device cookie (docs/security/login-rate-limiting.md):
-- lets a single cookie be revoked. Only the SHA-256 of the cookie's random id is stored.
CREATE TABLE device_cookies (
    id_hash      TEXT    PRIMARY KEY,
    username     TEXT    NOT NULL,
    issued_at    INTEGER NOT NULL,
    password_gen INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX device_cookies_user ON device_cookies(username, issued_at);
