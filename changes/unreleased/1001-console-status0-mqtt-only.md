---
section: Fixed
---
Console: a `Status 0` typed for a device reached over MQTT only is refused right away with the reason (Status 0 never goes over MQTT, and the device's HTTP is not known to work) and the hint to ask `Status 1` to `Status 11` instead, instead of being sent over HTTP and ending in "The device did not answer in time"; nothing is sent. Over a working HTTP it is sent as before after the confirmation. A bulk `Status 0` lists such devices as "Not sent" and leaves them out (#1001, thanks @Noschvie).
