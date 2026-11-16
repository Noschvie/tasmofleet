---
section: Added
---
Add devices can now say why a device that is on the broker is not offered as new: "Not listed? Look up by IP address, MAC or topic" in the Discovered box names the place it is in (ignored, with Restore; already in the device list, also when discovery bound it to a stored record without MAC and it keeps that record's name; listed as an address change or conflict; or seen on MQTT but not identified yet, with the reason), and sends nothing to a device or the broker. "Show ignored" now says that an ignored device stays hidden while the data it announces stays the same, and comes back when that changes or after 30 days without a sighting (#713, thanks @Noschvie)
