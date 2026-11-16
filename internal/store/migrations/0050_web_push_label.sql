-- 0050_web_push_label: Web Push, part 2 (#611): a name for each subscribed browser.
--
-- label is the browser and system the Settings list shows ("Chrome on Android"). The server makes it from the
-- User-Agent of the request that subscribed, through a short fixed table, so it is one of a few known words
-- or "Browser", never the raw header. Rows made before this migration read as '' and the list names them
-- "Browser". Additive only; the label ends with its row.
ALTER TABLE push_subscriptions ADD COLUMN label TEXT NOT NULL DEFAULT '';
