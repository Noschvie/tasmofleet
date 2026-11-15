---
section: Changed
---
The identity probe reads the device's MQTT broker, OTA URL, GroupTopic, DeviceName and relay names over MQTT first once a Status 5 on the device's topic reports its MAC; the firmware and hardware identity (Status 2, Status 4, the main page) and the MAC check of the address stay on HTTP (#851)
