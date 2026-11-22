---
section: Fixed
---
Status 9 on a device without energy monitoring that has SetOption4 1 is now the same Command Error over MQTT as over HTTP: the read no longer waits for the timeout, the device's next reads are no longer switched to HTTP, and a backup no longer lists Status 9 as not read when HTTP is unavailable. A failure of another Status (another client's, another index) no longer answers or fails TasmoFleet's own Status read or the Status 11 that confirms a light change. The simulator publishes such a failure where the firmware does, on RESULT (#1209)
