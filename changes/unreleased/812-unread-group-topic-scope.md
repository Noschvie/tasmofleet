---
section: Changed
---
A device whose group topic has not been read no longer keeps the changes, backups and restores of every other device on HTTP when it is provably not on TasmoFleet's broker (its MQTT settings name another broker or none, and nothing from it was ever heard on TasmoFleet's): the others go over MQTT first again. A device that may be on the broker still holds the others back, since its group topic could be any device's topic; the "How TasmoFleet reaches this device" card now names the devices that hold a device back, so you know whose identity to refresh (#812)
