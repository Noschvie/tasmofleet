---
section: Security
---

One rule decides what is secret, for the console, its replies and the audit as for the backup comparison, the drift view and the restore preview. Wi-Fi names (SSId, BSSId, RgxSSId) and the MQTT user are no longer shown in a backup comparison or the drift view; MqttUser, BSSId, WCAuth, TmChatId, IfxOrg, KeeloqSet, DzKeyIdx, RfKey and ZbNameKey now have their replies masked like the passwords and are refused as bulk commands. TLS fingerprints are shown and compared: a fingerprint is no secret. The Settings tab and the restore's MQTT check read the MQTT user from Status 6 (#809)
