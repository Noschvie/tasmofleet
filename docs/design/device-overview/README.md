# Device page Overview: target layout (#145)

Mockups of the reworked device Overview, one per device shape. They show the intended
layout and states, not final pixels; the issue has the rules.

| # | Device | Image |
| - | ------ | ----- |
| 1 | 1 relay, energy meter, Wi-Fi (plug) | [device-overview-1.png](device-overview-1.png) |
| 2 | 1 relay, no energy meter, Wi-Fi | [device-overview-2.png](device-overview-2.png) |
| 3 | 2 relays, one meter for both, Wi-Fi | [device-overview-3.png](device-overview-3.png) |
| 4 | 4 relays, metered per channel, weak Wi-Fi | [device-overview-4.png](device-overview-4.png) |
| 5 | 4 relays, no energy meter, Ethernet | [device-overview-5.png](device-overview-5.png) |
| 6 | Sensors only, no relay | [device-overview-6.png](device-overview-6.png) |
| 7 | HTTP only (no MQTT), one policy rule failing | [device-overview-7.png](device-overview-7.png) |
| 8 | Offline: the mockup draws the last reported state dimmed; the build says it in words (a "Last reported" label on the tiles and cards, no opacity on text, #1071), Reachability shows the current state without the label, device actions disabled, Edit/Delete/Activity active. The offline banner is one lead ("Offline since 2:17 PM.") and one sentence ("Controls are off until it is back."); the buttons that cannot act name it as their reason, and the header carries no second paragraph (#1082). A device that never answered is not offline: the badge and Reachability say Unknown, the one banner is "has not answered yet", Stability says "No data", and the Relays and Sensors cards wait for the first answer; a policy rule not read is "Not read", never part of "n / m" | [device-overview-8.png](device-overview-8.png) |
