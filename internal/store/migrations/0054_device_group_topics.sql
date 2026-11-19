-- 0054_device_group_topics: GroupTopic2 to GroupTopic4 of a device (#1005).
--
-- more_group_topics: the non-empty ones of GroupTopic2..4 as a JSON array of strings, '' for none (the encoding of
-- relay_names, 0031). GroupTopic1 stays in group_topic (0035), which INFO1 and Status 1 keep writing alone.
-- more_group_topics_read_at: unix ms of the bare GroupTopic read that stored the list; NULL never read. No backfill:
-- every device starts unread, so the shared-topic rule holds back MQTT writes until the reader has asked each device
-- once (fail closed). Written by the prober and the GroupTopic reader only.
ALTER TABLE devices ADD COLUMN more_group_topics TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN more_group_topics_read_at INTEGER;
