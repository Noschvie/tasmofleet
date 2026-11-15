---
section: Fixed
---
A restore over MQTT reads the device's TLS settings (SetOption103, SetOption132) also on a device with its own StateText words such as AUS/AN, so it no longer asks a lockout question or marks the device for a backup with the device's own settings (#845)
