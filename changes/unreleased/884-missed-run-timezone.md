---
section: Fixed
---
Scheduled backups and snapshots no longer report a run as missed, and no longer repeat it, after the container's time zone changed between the run and the restart (#884). The run record stores a moment, not the clock it was settled on, so a run of 03:00 under `TZ=CET` (02:00 UTC) read as one hour early when the container came back without `TZ` and looked at 03:00 UTC. TasmoFleet now remembers the clock it reads its schedules on and treats a change of `TZ` like saving the schedule: nothing starts, nothing is skipped, nobody is notified, and the next run is the next time of day on the new clock. Winter and summer time are not a change.
