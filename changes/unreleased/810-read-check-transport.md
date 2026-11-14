---
section: Fixed
---
The identity check after a read of a device's values now asks over the transport the read used: when a read of the Settings tab or of the policy check fell back from MQTT to HTTP, it could be answered by another device that now holds the address while the check over MQTT was answered by the right device, and the other device's values were shown and judged as this device's (and the same the other way round). A read answered over both transports is checked over both; a read no check vouches for is refused and nothing of it is kept. The Wi-Fi facts of a device (`Status 11`) are read over the transport its MAC check was answered on (#810)
