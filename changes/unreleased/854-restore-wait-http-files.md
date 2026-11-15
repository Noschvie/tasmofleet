---
section: Fixed
---
After a restore over MQTT that changed the MQTT connection, the check backup and the backups of the next ten minutes keep the device's files again: the HTTP polls the restarting device leaves unanswered no longer mark its HTTP as failing (#854)
