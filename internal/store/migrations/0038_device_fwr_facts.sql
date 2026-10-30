-- 0038_device_fwr_facts: the StatusFWR fields of Status 2 as the device sent them (Version,
-- BuildDateTime, Core, SDK, CpuFrequency, CR) for the Identity card (#289), as one JSON object.
-- Written by the prober with the identity; '' means never read (the card then shows nothing for
-- them until the next refresh). Hardware already has its own column.
ALTER TABLE devices ADD COLUMN fwr_facts TEXT NOT NULL DEFAULT '';
