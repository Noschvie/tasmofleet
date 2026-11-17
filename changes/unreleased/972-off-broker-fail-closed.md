---
section: Changed
---
A device whose group topic has not been read counts as "not on TasmoFleet's broker" (#812) only while TasmoFleet's MQTT connection has heard devices at all (not a broker that lets TasmoFleet send but not read) and no device outside the inventory is online on the broker (it may be one of yours that came back under a new topic): otherwise it holds the other devices' changes, backups and updates back to HTTP until its group topic is read (#972)
