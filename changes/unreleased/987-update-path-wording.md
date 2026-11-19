---
section: Fixed
---
The device page, the update preview and the first line of an update's log now word an update's path the same way, and no longer say the firmware download goes over MQTT: only the commands (OtaUrl and Upgrade) go over MQTT, the device fetches the image over HTTP from TasmoFleet's firmware server. The path's badge on the device page names the commands' transport ("Commands over MQTT" or "Commands over HTTP"), the note under it and the preview row say both parts ("commands over MQTT, the image over HTTP from TasmoFleet's firmware server"), and the preview now shows this note for the HTTP paths as well (#987).
