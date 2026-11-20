---
section: Fixed
---
A refresh of a device on Ethernet no longer queues a background network read that could only repeat the Status 5 the refresh had just stored. Over MQTT that read held the device's topic for the 1.5 second hear-all window and could delay the next refresh. A device that moves from Ethernet to Wi-Fi still gets its Wi-Fi facts, because the decision follows the refresh's fresh Status 5 and not the stored facts (#1015)
