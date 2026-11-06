# Safety rules that must never regress

Part of the working guide: start at [CLAUDE.md](../../CLAUDE.md).

1. `fwselect.Guard` runs immediately before every `OtaUrl` and `Upgrade`. It refuses on
   unknown hardware, chip-family mismatch (device vs artifact header vs file name), safeboot
   images, and variant changes without explicit user choice. Never bypass it, never "guess".
   Guard covers what TasmoFleet sends on its own (update jobs, restore, Make compliant). An
   `Upgrade`/`OtaUrl` typed in the console or sent as a bulk command is the person's
   responsibility after the confirmation (`cmdpolicy.whyFirmware`); the one hard rule there is
   that it is refused while a TasmoFleet update job holds the device (`cmdpolicy.StartsFirmware`).
2. `Status 0` is never sent by automated code (heap bursts corrupt ESP8266 replies). Use
   individual `Status N` queries, one at a time per device.
3. `Upgrade 1` is never retried blindly. After a lost response the engine probes first.
4. Device credentials never reach the browser, logs, errors or audit details.
5. Local accounts are managed by the CLI or by admins in the UI (`/users`) with step-up: the
   current password for local admins, the IdP re-authentication for OIDC admins. Header
   principals have no step-up and only view. Operators can never create accounts or raise
   roles. Listing needs no step-up, every change does.
6. Forwarded headers are trusted only from `TRUSTED_PROXIES` (checked on `RemoteAddr`).
