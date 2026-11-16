---
section: Changed
---
A device's language build is now read from the locale ID it reports in Status 4, over HTTP and over MQTT alike, instead of from its main page, so the identity probe and the update preview read no main page for normal firmware, and a device updated over MQTT no longer needs a language choice. Minimal and safeboot firmware are named by the INFO1 they publish after boot, and an update job through them by the chip it read at its start; the main page is read only for such a device outside a job when no INFO1 is there (#686, thanks @Noschvie)
