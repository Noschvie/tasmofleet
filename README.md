<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/branding/wordmark-dark.svg">
    <img src="assets/branding/wordmark.svg" alt="TasmoFleet" width="320">
  </picture>
</p>

<p align="center">
  <strong>Back up, check and update your Tasmota devices from one place, on your own server.</strong>
</p>

<p align="center">
  <a href="https://github.com/hellwand/tasmofleet/releases"><img alt="Release" src="https://img.shields.io/github/v/release/hellwand/tasmofleet?sort=semver"></a>
  <a href="https://hub.docker.com/r/nofuturekid/tasmofleet"><img alt="Docker Hub pulls" src="https://img.shields.io/docker/pulls/nofuturekid/tasmofleet?logo=docker&logoColor=white"></a>
  <a href="LICENSE"><img alt="License: AGPL-3.0-or-later" src="https://img.shields.io/github/license/hellwand/tasmofleet"></a>
  <a href="https://github.com/hellwand/tasmofleet/actions/workflows/main.yml"><img alt="CI" src="https://github.com/hellwand/tasmofleet/actions/workflows/main.yml/badge.svg?branch=main"></a>
</p>

<p align="center">
  <a href="https://hellwand.github.io/tasmofleet/">Website</a> ·
  <a href="https://hellwand.github.io/tasmofleet/demo/">Live demo</a> ·
  <a href="#quick-start-docker">Install</a> ·
  <a href="#documentation">Docs</a> ·
  <a href="https://github.com/hellwand/tasmofleet/releases">Release notes</a> ·
  <a href="https://github.com/hellwand/tasmofleet/discussions">Discussions</a>
</p>

![The TasmoFleet dashboard: fleet state, what needs attention, recent transactions](https://hellwand.github.io/tasmofleet/assets/dashboard.png)

TasmoFleet is a self-hosted fleet manager for [Tasmota](https://tasmota.github.io/) devices, one Go
binary with an embedded SQLite database. All screenshots show simulated devices; the
**[live demo](https://hellwand.github.io/tasmofleet/demo/)** is the real UI with a simulated fleet of 60
devices, saved as static pages (actions are disabled).

## What it does

- **Fleet overview**: every device with state, firmware and chip family, and a dashboard of what needs attention.
- **Guarded firmware updates**: server-side jobs that keep running when you close the browser, with a check before every upgrade.
- **Backups with diff and restore**: scheduled and before every update, compared side by side.
- **Settings policy with drift**: rules per chip family, tag or device, and "Make compliant" as one transaction.
- **Docker** and an **Unraid template**; a **Home Assistant app** is experimental and not published yet.

![A tour: the dashboard, a device behind the latest release, its backups, then its update from review to the running transaction](https://hellwand.github.io/tasmofleet/assets/tour.webp)

## Why TasmoFleet

Built on a home fleet of a few dozen devices, as an alternative to
[TasmoAdmin](https://github.com/TasmoAdmin/TasmoAdmin) (it imports its device list).

- **Backups with diff and restore**: the raw `.dmp` plus decoded JSON, scheduled and before every
  update, compared side by side, restored onto the same or a replacement device.
- **Policy and compliance**: rules such as `PowerOnState 0` or `TelePeriod 300` per chip family, tag
  or device, a drift view per device, and "Make compliant" as one transaction.
- **Safe bulk firmware updates**: persisted server-side jobs (closing the browser does not stop
  them), bounded parallelism, and a guard before every `OtaUrl` and `Upgrade` that refuses unknown
  hardware, a chip-family mismatch, safeboot images and a variant change you did not choose.
- **MQTT, optional**: live state and pages that update without reloading; without MQTT every
  feature works over HTTP polling.
- **Notifications** by webhook (for example Home Assistant) or email when an update finishes or a
  scheduled backup or snapshot fails.
- **Sign-in** with OIDC single sign-on, trusted forward-auth headers or local accounts; roles and an
  audit log of every change.
- **Nothing changes on a device on its own**: background work only reads. See
  [what TasmoFleet changes on your devices](docs/device-changes.md).

Also: discovery via MQTT or HTTP scan, Prometheus metrics, light and dark theme, phone layout,
import from TasmoAdmin.

| Device list                                                                   | Policy rule editor                                                                                 |
| ----------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| ![Device list with status, firmware and chip columns](https://hellwand.github.io/tasmofleet/assets/devices.png) | ![Policy rule editor suggesting commands from the Tasmota catalogue](https://hellwand.github.io/tasmofleet/assets/policy-editor.png) |

## Quick start (Docker)

```sh
docker run -d --name tasmofleet \
  -p 8443:8080 -p 80:80 \
  -e TZ=Europe/Berlin \
  -v tasmofleet-data:/data \
  nofuturekid/tasmofleet:latest
docker logs tasmofleet 2>&1 | grep -o '/setup/account?token=[^"]*'
```

1. Open the setup link: `https://<host>:8443` followed by the `/setup/account?token=...` path printed once
   at start in the TasmoFleet log (`docker logs tasmofleet`; on Unraid, open the container's log). Accept
   the self-signed certificate and set the admin password.
2. In the setup, enter your MQTT broker first (devices then report at once and changes go over MQTT
   first; HTTP is the fallback), then your device networks, and choose **Finish setup**.
3. On the empty Devices page, open **Add devices** and add your devices.
4. Under **Device backups**, choose **Back up all now** to take the first backup.

Port 80 serves firmware to the devices over plain HTTP; set the URL they use on the System settings
page.

With Docker Compose:

```yaml
services:
  tasmofleet:
    image: nofuturekid/tasmofleet:latest
    container_name: tasmofleet
    restart: unless-stopped
    ports:
      - "8443:8080" # UI (HTTPS, self-signed; container port is always 8080)
      - "80:80" # firmware for the devices (plain HTTP)
    environment:
      TZ: "Europe/Berlin"
    volumes:
      - tasmofleet-data:/data
volumes:
  tasmofleet-data:
```

The setup link is then in `docker compose logs tasmofleet`. Images are on Docker Hub and
`ghcr.io`, for amd64 and arm64. Bind mounts, registries, architectures and upgrading:
[docs/installation.md](docs/installation.md).

**Unraid**: install **TasmoFleet** from Community Applications (search for it in the Apps tab).
A manual template is in [deploy/unraid/tasmofleet.xml](deploy/unraid/tasmofleet.xml).

**Home Assistant app**: experimental and not published yet (no sidebar entry, no Home Assistant
sign-in). The app and its documentation are in
[deploy/homeassistant/tasmofleet](deploy/homeassistant/tasmofleet/DOCS.md).

## MQTT and the broker ACL

> [!CAUTION]
> **Set up an ACL on your MQTT broker before you use TasmoFleet's MQTT features.** MQTT is the first way TasmoFleet
> talks to a device that is online on its broker (reads, changes, backups and restores). The broker hands every command,
> every password in it and the complete configuration dump of every backed-up or restored device to **every client
> that may subscribe to the device's topics**: Home Assistant, Node-RED, MQTT Explorer, other devices. Without an ACL
> that is every client of the broker. A ready, tested rule set is in the [broker ACL guide](docs/mqtt-broker-acl.md);
> note that the Mosquitto app of Home Assistant currently does not enforce an `acl_file` (the guide says what to do).

## Backing up TasmoFleet

Back up the whole data directory (the `/data` volume): it holds the database and the device
backups. System, Snapshots downloads a consistent database snapshot, and scheduled snapshots are
possible. How to take, schedule and restore them:
[docs/backing-up.md](docs/backing-up.md).

## Documentation

- [Installing](docs/installation.md), [upgrading](docs/upgrading.md), [configuration](docs/configuration.md)
  (every environment variable, TLS, time zone, Home Assistant app)
- [Broker ACL guide](docs/mqtt-broker-acl.md): who may read what on your MQTT broker (read this before MQTT)
- [Backing up TasmoFleet](docs/backing-up.md) and [emergency access](docs/runbooks/emergency-access.md)
- [What TasmoFleet changes on your devices](docs/device-changes.md), including the two roles
- Reverse proxy, OIDC ([Authentik](deploy/authentik/README.md), [Keycloak](deploy/keycloak/README.md)), Prometheus, Grafana and Unraid examples: [deploy/](deploy/)
- Design and behaviour: [spec](docs/spec/2026-10-18-tasmofleet-design.md); security notes per
  feature: [docs/security/](docs/security/); research: [docs/research/](docs/research/)
- [CHANGELOG](CHANGELOG.md), [ROADMAP](ROADMAP.md), [development](docs/development.md)

## Community projects

Unofficial projects by other people. They are not maintained or endorsed by TasmoFleet.

- [tasmofleet-keycloak-theme](https://github.com/Noschvie/tasmofleet-keycloak-theme): a Keycloak
  login theme in the TasmoFleet look (light and dark), for people who use Keycloak as their OIDC
  provider. AGPL-3.0-or-later.

## Status

**Pre-1.0, single maintainer.** It runs a home fleet of a few dozen devices every day, but breaking
changes are to be expected before 1.0: read the [CHANGELOG](CHANGELOG.md) before updating. Your data
is migrated forward; configuration and routes may change. Questions and ideas are welcome in
[Discussions](https://github.com/hellwand/tasmofleet/discussions), bug reports in the
[issues](https://github.com/hellwand/tasmofleet/issues).

## How it is built (AI disclosure)

TasmoFleet is built with [Claude Code](https://claude.com/claude-code): most code, tests and docs
were written by it. The maintainer sets direction, reviews the changes and tests them on a real
fleet of Tasmota devices. The safety-critical parts (firmware selection, the OTA state machine) run
under mutation testing in addition to unit, integration and Playwright tests against a device
simulator.

TasmoFleet is an independent project and is not affiliated with, endorsed by or sponsored by
Tasmota or its maintainers. "Tasmota" is the name of the firmware this tool manages; the TasmoFleet
logo is original and unrelated to the Tasmota logo.

## License

[GNU Affero General Public License v3.0 or later](LICENSE) (`AGPL-3.0-or-later`). If you run a
modified version as a network service, you must offer its source to its users; the About page links
to the source code. Third-party notices are in [NOTICE](NOTICE).
