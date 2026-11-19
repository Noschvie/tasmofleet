---
section: Fixed
---
Saving a device without changing any of its fields (for example when only the MQTT password mode changes) no longer writes the device again or adds an empty `device.update` row to the audit log; the password-mode entry stays, and a device without tags that is announced again unchanged no longer adds one either (#988).
