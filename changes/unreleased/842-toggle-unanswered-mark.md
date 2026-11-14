---
section: Fixed
---
A relay switch over MQTT that the device does not answer now sends the device's next switches and commands over HTTP for a while, like any other unanswered change over MQTT, instead of publishing every further switch into the void while the device's last will still says Online (#842)
