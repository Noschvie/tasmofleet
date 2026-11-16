# Backing up TasmoFleet

Back up the whole data directory (the `/data` volume): it holds the database and the device
configuration backups (`.dmp`). Before every database migration TasmoFleet writes a copy next
to the database, `<db>.pre-<N>` (the newest three are kept), so an upgrade can be undone.

**System, Snapshots** (admins only, after a password confirmation, or a sign-in at your identity provider; a proxy-header sign-in ticks a checkbox instead) downloads
a consistent snapshot of the database, made with SQLite `VACUUM INTO` while TasmoFleet runs. The
file holds every user's password hash and the encrypted device, broker and OIDC secrets; the key
that decrypts them, `secret.key`, is a separate file and is only included (as a `.tar`) when you
tick the box. Device backups (`.dmp`) and the firmware cache are not part of it. The same page can
keep scheduled snapshots in `/data/snapshots` (`tasmofleet-YYYYMMDD-HHMMZ.db`; `SNAPSHOT_DAYS`, `SNAPSHOT_TIME`, `SNAPSHOT_KEEP`; off by default).
**Create snapshot now** on the same page takes one on request: it is written to the same folder, with the same name
and the same `SNAPSHOT_KEEP` retention as a scheduled one, so it counts toward the number you keep and can push the oldest
one out. It needs no password, because the file stays on the server, and it does not change the schedule's status
(a failed scheduled snapshot stays shown as failed). A second one in the same minute replaces the first, because names
are by the minute.
The time in a file name is UTC, which the `Z` marks, so the names stay unique and in order when the clocks change in
autumn: `tasmofleet-20261106-0800Z.db` was taken at 09:00 in Central European Time (10:00 in summer time).
`SNAPSHOT_TIME` is on the container clock, and the list on the Snapshots page shows each snapshot on your browser's clock.
They sit on the same disk as the database, so they protect against a bad upgrade or a deleted row,
not against a dead disk: keep a copy elsewhere.

To restore (there is no restore button on purpose):

1. Stop the container.
2. Replace `/data/tasmofleet.db` with the snapshot (or a `tasmofleet.db.pre-<N>` copy) and delete
   `tasmofleet.db-wal` and `tasmofleet.db-shm` next to it. From a `.tar`, unpack `tasmofleet.db`
   and `secret.key` into `/data`.
3. Make sure `/data/secret.key` is the one from the same time, then start the container. A
   snapshot from an older version is migrated at start; one from a newer version is refused.
