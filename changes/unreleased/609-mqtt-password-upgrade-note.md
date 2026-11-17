---
section: Upgrade notes
---
The first start of this version reads each device's MqttPassword from its newest decoded backup and audits every change as `device.mqtt_password` (source `backup`). A password typed before the upgrade counts as typed at the device's last change, so only a backup taken after that replaces it. To keep every password exactly as it is, start this version with `TASMOFLEET_MQTT_PASSWORD_FROM_BACKUPS=false` (imported into the setting once, then edited on System, MQTT). Devices whose MQTT password was "Not set" then back up and restore over MQTT, so their settings dumps (with their Wi-Fi, web and MQTT passwords, only obfuscated) cross the broker: check the broker ACL (System, MQTT) before the upgrade, or start with the switch off as above (#609)
