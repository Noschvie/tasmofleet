---
section: Fixed
---
A backup over MQTT of a device with no MAC in the inventory says "the inventory has no MAC for this device (probe it first)", as the restore does, instead of "the device at its address is not this device", and asks nothing over HTTP then, also while HTTP is known to fail; a host that reports a MAC made only of separators is never taken for the device by the MAC check of a backup or a restore (#846)
