---
section: Added
---
The device page's "Device MQTT password" row now says one word (Works, Refused, Not set, Not known, Not tested, Not needed), where the password came from and when, and what it enables (back up, restore, read files and upload firmware over MQTT), with Test and Change…. A password typed or set in bulk shows "Not tested" until the device accepts it: Test asks it, and so does the next finished backup over MQTT. System, MQTT has a new card with the counts, the devices without a password (with the reason and Set…) and "Use the broker password for these…", which sets several devices in one step and tests each one in the background; the password itself is never shown, logged or audited (#609, #457)
