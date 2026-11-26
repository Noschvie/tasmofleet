---
section: Changed
---
The Devices list in plain words: the "MQTT · HTTP" column is "Reachable via" and names the channels that carry a device now ("MQTT", "Web", "MQTT, Web") instead of check marks, the search field shows an example ("Name, address or MAC"), Offline and Unknown are two chips that match the dashboard tiles (the tiles open them; the old "Offline / unknown" chip is gone, and a device whose version was only read over MQTT and that never answered is Unknown everywhere), and "Back up now" within the cooldown shows one warn banner, "Too many requests. Try again in a few seconds.", instead of a red toast with timing words (#1132). The row menu has "Open web UI", which opens the device's own page in a new tab (#1204, thanks @Noschvie)
