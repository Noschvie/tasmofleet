-- 0044_backup_run_item_since: when a skipped device went offline (#409). The scheduled run used to
-- store the reason as a text with a hand-formatted time ("offline since 3 Nov 21:14"); the reason is now
-- a plain word and the time its own column, so the pages show it like every other time (their zone and
-- format). 0 = no time (never answered, busy, and every item stored before this migration).
ALTER TABLE backup_run_items ADD COLUMN since_at INTEGER NOT NULL DEFAULT 0;
