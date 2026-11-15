---
section: Fixed
---
A restore over MQTT of a device whose topic holds placeholders (Tasmota's default tasmota_%06X, %hostname% and the others) compares the backup's topic setting with the one the device reports, placeholders and all (and the hostname when the topic holds %hostname%), so its own topic no longer counts as a change of the MQTT connection (#844)
