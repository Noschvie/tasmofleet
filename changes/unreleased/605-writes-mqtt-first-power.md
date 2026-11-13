---
section: Upgrade notes
---
The switches of a device on TasmoFleet's broker whose group topic has not been read yet (or while another device's has not) now go over HTTP, like every other change: its topic might be shared, and a switch over MQTT could reach other devices. If HTTP does not reach such a device either, switching it fails with the reason until that group topic is read. Refreshing the identity of the device it belongs to reads it; the device page's "How TasmoFleet reaches this device" card says whether that is this device or another one (#605)
