---
section: Fixed
---
When another device answers on a device's MQTT topic, TasmoFleet now stops sending the device's commands there: its reads go over HTTP while HTTP works, and changes, backups and restores never go to that topic, until the device answers alone on its topic again (TasmoFleet asks the topic every 10 minutes until it is free, with a check that hears every reply; Refresh identity asks too) or its topic changes. A restore over MQTT that hears another device on the topic marks it the same way, and the device page's MQTT row then says "Topic taken by another device". The log warns about it with the topic's name, also from the data refresh, which wrongly said "another device answers at the address" at debug level (#852)
