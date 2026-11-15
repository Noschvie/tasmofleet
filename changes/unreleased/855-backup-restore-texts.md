---
section: Fixed
---
Four backup and restore texts that were wrong or misleading: a backup over MQTT asks the device on its topic whether it has a file system, so a device without one says "no filesystem" and is asked nothing over HTTP (it was said to need HTTP for its files when HTTP was down), a restore that cannot compare the settings names which backup lacks the decoded copy (or that the copy could not be read) instead of "decoder missing", the restore confirmation says "The 1 file is written", and the audit entry of a backup names the transport of the dump ("mqtt") and lists the HTTP calls beside it (MAC check, files) apart (#855)
