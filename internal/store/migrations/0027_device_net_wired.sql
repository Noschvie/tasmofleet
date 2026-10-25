-- 0027_device_net_wired: 1 when the device is on Ethernet (Status 5 reports an Ethernet address
-- while the Wi-Fi station has none). The net_* address fields then hold the Ethernet values.
ALTER TABLE devices ADD COLUMN net_wired INTEGER NOT NULL DEFAULT 0;
