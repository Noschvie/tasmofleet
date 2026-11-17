---
section: Breaking
---
The setting behind the option is renamed from `MQTT_ACL_CONFIRMED` to `MQTT_ACL_WARNING_HIDDEN`, without an alias. A value stored in the database moves to the new name with the upgrade (a hidden warning stays hidden for the same broker); the variable `TASMOFLEET_MQTT_ACL_CONFIRMED` is ignored, with a warning at start. Delete it (#962)
