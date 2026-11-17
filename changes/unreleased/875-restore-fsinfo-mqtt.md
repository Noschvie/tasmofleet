---
section: Changed
---
A restore over MQTT asks the device on its topic whether it has a file system, as the backup does, before it asks anything over HTTP: a device without one (or with firmware too old for files) is asked nothing over HTTP, and the confirmation page and the job say "the device has no flash file system" instead of "files need HTTP"; the files themselves still go over HTTP (#875)
