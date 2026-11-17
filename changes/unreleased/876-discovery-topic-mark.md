---
section: Fixed
---

MQTT: a discovery config that announces another MAC on a stored device's topic now makes the broker reader check that topic (the router's own check, which hears every reply, at most once per device per 10 minutes and two per 30 s pass), so a second board there marks the device and keeps its writes and transfers off the topic, while a forged config that only the device answers marks nothing (#876)
