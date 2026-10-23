-- 0025_initial_setup (design v2, N12): marks the admin account the first
-- start created while it still waits for its first password; only then is
-- the setup link from the container log valid. Set only when that account is
-- inserted, cleared by every change that increments password_gen. Existing
-- rows get 0: they never take the setup link.
ALTER TABLE local_users ADD COLUMN initial_setup INTEGER NOT NULL DEFAULT 0;
