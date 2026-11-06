# What TasmoFleet changes on your devices

Nothing on its own. Everything TasmoFleet does in the background only reads: status polling
(single `Status N` queries, never `Status 0`), the policy live check, scheduled backups (config
download and status snapshot), and discovery (MQTT announcements, HTTP scan).

A device is only changed when someone starts an action in the UI: a firmware update, "make
compliant" for the policy, a console or bulk command, a power toggle, a restart, a restore or a
settings edit. Each of these is recorded in the audit log.

**TasmoFleet leaves the power state alone.** It does not read or switch relays around a firmware
update. A device that loses its relay state when it reboots (a light driver that switches itself off after the boot, an ESP32 without `SetOption0 1`)
comes back as its own settings say. To have it come back as it was, set `SetOption0 1` (save
the relay state), `PowerOnState 3` (keep the last state) and `SaveData 1` on the device; a policy
rule can do that for the fleet (the "Restore power after an outage" template covers `PowerOnState 3`).

There is no read-only mode or view-only role yet: both roles can change devices. Admins can do
everything; operators can run updates, backups, power toggles, restarts and the allowlisted
commands, but cannot edit the inventory, restore backups, manage firmware, edit or fix policy,
edit settings or manage users.
