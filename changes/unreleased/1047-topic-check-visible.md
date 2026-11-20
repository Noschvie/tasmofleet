---
section: Added
---
The shared-topic check before an MQTT write is now visible: a Topic check card under System → Settings → MQTT says whether all device topics are unique, lists every collision with the devices involved (a GroupTopic equal to another device's Topic, two equal Topics, overlapping command topics, a Topic equal to `tasmotas`) and, apart from them, the devices not fully read yet; the Identity card of a device whose MQTT writes are held back names the colliding topic and the other device. A GroupTopic several devices share, such as `tasmotas`, is no collision. Right after a start or an upgrade the card lists devices as not fully read yet until the background reader has asked each one (one device every 2 seconds) (#1047, thanks @Noschvie)
