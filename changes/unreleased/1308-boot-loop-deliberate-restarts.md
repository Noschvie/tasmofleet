---
section: Fixed
---
A device that was restarted on purpose outside TasmoFleet (plugged in, MQTT settings sent over HTTP, a template change) is no longer shown as "Boot loop" when it ran for 2 minutes or more before each restart. A crash, watchdog or brown-out restart, short runs and the Wi-Fi config reboot cycle still count (#1308)
