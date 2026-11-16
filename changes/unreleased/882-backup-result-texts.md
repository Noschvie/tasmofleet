---
section: Fixed
---
The Back up button on the device page now says what happened, in the words of the bulk result and the transaction page: "Backup taken over MQTT (config, status, decoded, files)" instead of only "Backup taken.", and the Activity box shows the same note for a succeeded backup. A restore that checks one file now reads "the 1 file matches the backup" and "1 file differs from the backup" instead of "match" and "differ" (#882).
