---
section: Added
---
A big red notice says, wherever MQTT settings and MQTT-carried secrets appear, that without an ACL on the broker every MQTT client can read the commands, passwords and device backups TasmoFleet sends through it: on System, MQTT and the setup's broker section, by the device's MQTT password, on the confirmation of a console or bulk command that goes over MQTT, of a restore over MQTT and on the page that starts bulk backups. The new guide docs/mqtt-broker-acl.md has a ready ACL for TasmoFleet, the devices and Home Assistant with the reason for every line, a way to check it, a stricter one-login-per-device variant and sections for plain Mosquitto and EMQX; its rules were run against a real Mosquitto 2.1.2 (make mqtt-acl-check) (#840)
