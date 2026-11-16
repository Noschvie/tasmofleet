---
section: Changed
---

A device on TasmoFleet's broker now has its identity (firmware version, chip, flash size, language) read over MQTT first, also while its web server is on, by Refresh identity and by the background identity refresh, which now also reaches a device whose HTTP API is off, every 24 hours and after a restart. The identity is taken from MQTT only when exactly one device answers the MAC check on the topic, with the stored MAC; otherwise the device's address is asked as before. A device whose web server was switched off after it was added therefore no longer keeps the version, flash size and language of its last HTTP probe. Every such refresh waits 1.5 s for a second answer on the topic (#682)
