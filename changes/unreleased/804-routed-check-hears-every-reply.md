---
section: Fixed
---

MQTT: the identity checks that vouch for reads over MQTT (the background data refresh and broker read, the network facts read, the Settings tab and the policy poller) now hear every reply on the device's topic within the 1.5 s identity window instead of the first one, so a second device answering on the same topic (with an error too, as a board in minimal firmware does, or without a MAC) refuses the read, nothing of it is cached, the device is marked as sharing its topic and the device page says why its data was not refreshed; when that check learns nothing, the check goes to the device's address alone (#804, #878)
