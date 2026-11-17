---
section: Added
---
Backups and restores over MQTT need each device's MqttPassword, and TasmoFleet now takes it from the device's newest decoded backup: after every backup, and once over the existing backups at start. A password equal to TasmoFleet's broker password becomes "Same as the broker's", an empty one the new choice "No password" (a device without an MqttPassword could not be backed up over MQTT before), anything else is stored sealed like a typed one. The newer fact wins: a password typed after the backup stays until the next backup. Only the source and the backup's date are shown in the audit, never the value. On by default; System, MQTT, "Take device passwords from backups" turns it off (#609)
