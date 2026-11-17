---
section: Fixed
---

MQTT: a light change (Dimmer, Color, Channel, HSBColor, White, CT) sent over MQTT is no longer reported done on the strength of another light change on the same device; TasmoFleet reads the light's state with Status 11 right after it and reports that. Channel2, Color2, HSBColor2 and Dimmer2/3 over MQTT no longer time out and send the device's next commands over HTTP for ten minutes. A Backlog with a light change gets a Status 11 as its last part, or goes over HTTP once when there is no room for it (#813)
