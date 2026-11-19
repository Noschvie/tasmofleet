---
section: Changed
---
Backups: every backup run now starts its devices in name order, A to Z (case-insensitive, then by device ID), the same way the scheduled run always did. "Back up all now" used to start them in the order the devices were added and "Back up selected" in the order of the device list. The result page of a backup run lists the devices in that start order (the run still does two at a time, so the finish times can differ slightly), and the run's rows and the Backups table show the device ID as a small muted "#N" next to the name (#993, thanks @Noschvie).
