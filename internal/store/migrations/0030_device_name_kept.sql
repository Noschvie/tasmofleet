-- 0030_device_name_kept: the DeviceName the admin answered "Keep" to (#125). A name set by hand is
-- never overwritten by a refresh; the device page offers the DeviceName the device reports instead,
-- and "Keep" remembers which one was declined so the offer only returns when the device reports
-- another. Empty: nothing declined.
ALTER TABLE devices ADD COLUMN device_name_kept TEXT NOT NULL DEFAULT '';
