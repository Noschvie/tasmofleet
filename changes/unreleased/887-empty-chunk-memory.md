---
section: Changed
---
A backup that fails because the device sent an empty data chunk now says that the device ran short of contiguous memory and that a restart usually helps, instead of "did not hand over its configuration"; a failed backup that the device answered records its free heap and uptime, read with one Status 11 right after the failure, and the Backups page shows them on the failed row (#887)
