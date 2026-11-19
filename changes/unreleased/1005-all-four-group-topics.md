---
section: Fixed
---
The check that keeps a write or a transfer off MQTT when another device could receive it now knows all four group topics of every device, not only GroupTopic1: TasmoFleet reads GroupTopic1 to GroupTopic4 with one bare `GroupTopic` (MQTT first), stores them, and the device list's Group topic column shows the first one with "+N" for the others (all of them on hover), the list filter finds a device by any of them, and the Identity card lists them all. After TasmoFleet itself sets a group topic (console, bulk command) or restores a configuration, the device's group topics count as unread until they have been read again, a few seconds later (#1005, thanks @Noschvie).
