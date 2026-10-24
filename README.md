<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/branding/wordmark-dark.svg">
    <img src="assets/branding/wordmark.svg" alt="TasmoFleet" width="320">
  </picture>
</p>

# TasmoFleet

A self-hosted fleet manager for [Tasmota](https://tasmota.github.io/) devices: inventory, live
state, firmware updates, configuration backups and fleet-wide commands in one Go binary with an
embedded SQLite database.

[![CI](https://github.com/nofuturekid/tasmofleet/actions/workflows/ci.yml/badge.svg)](https://github.com/nofuturekid/tasmofleet/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/nofuturekid/tasmofleet?sort=semver)](https://github.com/nofuturekid/tasmofleet/releases)
[![License: AGPL-3.0-or-later](https://img.shields.io/github/license/nofuturekid/tasmofleet)](LICENSE)
[![Image: ghcr.io](https://img.shields.io/badge/image-ghcr.io%2Fnofuturekid%2Ftasmofleet-blue?logo=docker&logoColor=white)](https://github.com/nofuturekid/tasmofleet/pkgs/container/tasmofleet)

![A firmware update of four devices: pick them, review, start, watch each device's progress](docs/media/demo.webp)

**[Try the demo](https://nofuturekid.github.io/tasmofleet/)**: the real UI with a simulated fleet
of 60 devices, saved as static pages (actions are disabled). The
[design prototype](https://nofuturekid.github.io/tasmofleet-design/) the UI was built from is
public too.

TasmoFleet is an independent project and is not affiliated with, endorsed by or sponsored
by Tasmota or its maintainers. "Tasmota" is the name of the firmware this tool manages; the
TasmoFleet logo is original and unrelated to the Tasmota logo.

## Why

TasmoFleet started as a replacement for [TasmoAdmin](https://github.com/TasmoAdmin/TasmoAdmin)
on a home fleet of a few dozen devices. What it does differently:

- **Updates run on the server**, as a persisted job per device (backup, OTA URL, upgrade,
  wait for the reboot, verify), with bounded parallelism. Closing the browser does not stop an
  update, and an update interrupted by a restart resumes. TasmoAdmin drives updates from the
  open browser tab.
- **A safety guard before every `OtaUrl` and `Upgrade`**: it refuses unknown hardware, an image
  whose chip family does not match the device (checked against the image header and the file
  name), safeboot images, and a variant change you did not choose explicitly. 1 MB ESP8266
  devices go through `tasmota-minimal` first.
- **Live state over MQTT** (optional): online state, power and telemetry arrive as they happen
  and the pages update without reloading. Without MQTT every feature works over HTTP polling.
- **Policy and compliance**: rules such as `PowerOnState 0` or `TelePeriod 300` per chip family,
  tag or device, a drift view per device, and "make compliant" as a transaction.
- **Backups with diff and restore**: the raw `.dmp` plus decoded JSON (with
  [decode-config](https://github.com/tasmota/decode-config), bundled in the image), scheduled
  and before every update, compared side by side, restored onto the same or a replacement device.
- **Bulk actions as transactions with logs**: updates, commands, backups and policy fixes on
  many devices run as one transaction, with a log per device, cancel and retry of the failed ones.
- **Sign-in** with OIDC single sign-on, trusted forward-auth headers, or local accounts; roles
  (admin, operator) and an audit log of every change.
- **Import from TasmoAdmin**: `devices.csv` and `MyConfig.json`, in the UI or with
  `tasmofleet import tasmoadmin`.

## Features

- **Inventory and discovery**: devices found via MQTT announcements or an HTTP scan of your
  device networks are proposed, not added silently; address changes are detected.
- **Firmware** from upstream (GitHub releases, ota.tasmota.com) or your own uploads, cached,
  checked by its image header and served to devices over plain HTTP from a separate port.
- **Device pages**: overview, power control, console with a command policy, settings, backups,
  drift, state and transaction history.
- **Notifications** of finished updates and failed backups by webhook (e.g. a Home Assistant
  webhook trigger) or e-mail (Resend API).
- **Prometheus metrics** and an example Grafana dashboard ([deploy/](deploy/)).
- Light and dark theme, phone layout, works behind a reverse proxy under a base path.
- Automation stays in Home Assistant (or wherever you run it): TasmoFleet manages devices, it
  does not switch them on schedules.

### What TasmoFleet changes on your devices

Nothing on its own. Everything TasmoFleet does in the background only reads: status polling
(single `Status N` queries, never `Status 0`), the policy live check, scheduled backups (config
download and status snapshot), and discovery (MQTT announcements, HTTP scan).

A device is only changed when someone starts an action in the UI: a firmware update, "make
compliant" for the policy, a console or bulk command, a power toggle, a restart, a restore or a
settings edit. Each of these is recorded in the audit log.

There is no read-only mode or view-only role yet: both roles can change devices. Admins can do
everything; operators can run updates, backups, power toggles and the allowlisted commands, but
cannot edit the inventory, settings or users.

| Dashboard                                                                                | Device list                                                                   |
| ---------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| ![Dashboard with fleet state, attention list and transactions](docs/media/dashboard.png) | ![Device list with status, firmware and chip columns](docs/media/devices.png) |
| **Running update transaction**                                                           | **Device page (dark theme)**                                                  |
| ![A transaction with per-device progress](docs/media/transaction.png)                    | ![Device overview in the dark theme](docs/media/device-dark.png)              |

All screenshots show simulated devices.

## Quick start (Docker)

```sh
docker run -d --name tasmofleet \
  -p 8443:8443 -p 80:80 \
  -e TASMOFLEET_LISTEN=:8443 \
  -e TASMOFLEET_TLS_SELF_SIGNED=true \
  -v tasmofleet-data:/data \
  ghcr.io/nofuturekid/tasmofleet:latest
docker logs tasmofleet 2>&1 | grep -o '/setup/account?token=[^"]*'
```

Open `https://<host>:8443` followed by the printed `/setup/account?token=...` path, accept the
self-signed certificate, set the admin password and follow the setup. The link is valid until
the admin has a password; each start prints a fresh one until then.

Port 80 serves firmware to the devices (they fetch it over plain HTTP); it can be any port the
devices can reach. Set the URL the devices use (e.g. `http://<host-ip>`) on the System settings
page; until then firmware updates are off.

The same with Docker Compose:

```yaml
services:
  tasmofleet:
    image: ghcr.io/nofuturekid/tasmofleet:latest
    restart: unless-stopped
    ports:
      - "8443:8443" # UI (HTTPS, self-signed)
      - "80:80" # firmware for the devices (plain HTTP)
    environment:
      TASMOFLEET_LISTEN: ":8443"
      TASMOFLEET_TLS_SELF_SIGNED: "true"
    volumes:
      - tasmofleet-data:/data
volumes:
  tasmofleet-data:
```

With Compose, the setup link is in `docker compose logs tasmofleet`.

**Unraid**: a template is in [deploy/unraid/tasmofleet.xml](deploy/unraid/tasmofleet.xml) (copy
it to `/boot/config/plugins/dockerMan/templates-user/`); it runs plain HTTP behind a reverse
proxy with its own IP on `br0`.

## Configuration

The environment only holds bootstrap settings (listeners, data directory, trusted proxies,
sign-in mode); everything else, single sign-on included, is set in the UI and stored in the
database. A value set in the environment locks its field in the UI.

- All variables: [deploy/unraid/tasmofleet.env.example](deploy/unraid/tasmofleet.env.example)
- Reverse proxy, OIDC (Authentik), Prometheus, Grafana and Unraid examples: [deploy/](deploy/).
  Behind a reverse proxy, run plain HTTP on `:8080` with `TASMOFLEET_TRUSTED_PROXIES` and
  `TASMOFLEET_PUBLIC_URL`.
- Lost passwords and emergency access: [docs/runbooks/emergency-access.md](docs/runbooks/emergency-access.md)

## Status

**Pre-1.0 (v0.11.x), single maintainer.** It runs a home fleet of a few dozen devices every
day, but expect breaking changes before 1.0; they are listed in [CHANGELOG.md](CHANGELOG.md).
Your data is migrated forward; configuration and routes may change. Bug reports and questions
are welcome in the [issues](https://github.com/nofuturekid/tasmofleet/issues).

## Documentation

- Design and behaviour: [docs/spec/2026-10-18-tasmofleet-design.md](docs/spec/2026-10-18-tasmofleet-design.md)
- Security notes per feature: [docs/security/](docs/security/)
- Research (Tasmota OTA behaviour, TasmoAdmin lessons): [docs/research/](docs/research/)
- Roadmap: [ROADMAP.md](ROADMAP.md)

## Development

```sh
make check   # templ, fmt, vet, staticcheck, golangci-lint, unit tests, govulncheck, build
make ci      # everything CI runs, incl. race, integration, mutation and e2e tests
make demo    # the static demo (dist/demo); make demo-media renews docs/media
```

Requires Go 1.27. The e2e tests need Node.js (`make e2e-deps`, then
`npx playwright install chromium` once). See [CONTRIBUTING.md](CONTRIBUTING.md) and
[CLAUDE.md](CLAUDE.md) (rules for AI agents and humans).

## How it is built (AI disclosure)

TasmoFleet is built with [Claude Code](https://claude.com/claude-code): most code, tests and
docs were written by it. The maintainer sets direction, reviews the changes and tests them on a
real fleet of Tasmota devices. The safety-critical parts (firmware selection, the OTA state
machine) run under mutation testing in addition to unit, integration and Playwright tests
against a device simulator.

## License

[GNU Affero General Public License v3.0 or later](LICENSE) (`AGPL-3.0-or-later`). If you
run a modified version as a network service, you must offer its source to its users; the
About page links to the source code. Third-party notices are in [NOTICE](NOTICE).
