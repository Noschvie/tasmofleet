-- 0031_device_overview_facts: what the device Overview shows beyond identity (#145, #127): the
-- subnet mask and DNS servers of the active interface (Status 5), the OtaUrl without credentials
-- (Status 1) and the relay names (FriendlyName array, a JSON array like tags). All are read by
-- the prober only. Empty: not read yet (devices probed before this migration).
ALTER TABLE devices ADD COLUMN net_subnet TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN net_dns TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN ota_url TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN relay_names TEXT NOT NULL DEFAULT '';
