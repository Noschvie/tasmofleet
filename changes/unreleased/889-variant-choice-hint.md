---
section: Fixed
---
The update preview now says what to do for every device that is refused because its firmware is custom or unrecognised (an ESP8266 vendor build such as tasmota-NOUS_A1T_DE, a single-core ESP32, an ESP32-P4): go Back and enter the Variant override "tasmota". Before, only an ESP32-S2 row said so, and the other rows showed just the raw refusal (#889)
