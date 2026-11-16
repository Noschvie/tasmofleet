---
section: Fixed
---
The Back up result on the device page, the bulk result, the transaction page and the Activity box no longer say "over MQTT" for a backup whose files were read over HTTP: the text names both, "Backup taken over MQTT, files over HTTP (config, status, decoded, files)", and keeps "over HTTP" or "over MQTT" alone when the whole backup used one transport or stored no files archive (#926).
