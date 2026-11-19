---
section: Upgrade notes
---
Right after the upgrade, GroupTopic2 to GroupTopic4 are unread for every device. Like any unread group topic, that holds back writes and transfers over MQTT for the whole fleet until TasmoFleet has asked each device once (one device every 2 seconds, so about a minute and a half for 45 devices); in that window they go over HTTP where HTTP reaches the device (#1005, thanks @Noschvie).
