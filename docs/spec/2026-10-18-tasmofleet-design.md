# tasmofleet design

Status: accepted 2026-10-18, implemented; every statement about current behaviour was checked against the code of 0.19.2 on 2026-10-30. Owner: the maintainer. Author: architect agent.
The code in `internal/` implements the contracts in this document. If code and
spec disagree, fix one of them in the same change.

Inputs: device captures in `testdata/fixtures/` (Tasmota 16.0.0, 6 devices, 6.7 min of
MQTT), `docs/research/tasmota-minimal-and-ota.md` (minimal firmware and OTA flow, derived
from the v16.0.0 source), and `docs/research/tasmoadmin-*.md` (feature inventory, data
formats, lessons learned). Labels used below: **OBS** = observed on real devices, **SRC** =
read from Tasmota source, **DEC** = design decision.

---

## 1. Purpose and scope

tasmofleet manages a fleet of Tasmota devices and replaces TasmoAdmin with feature
parity. Home Assistant keeps automation. tasmofleet does inventory, live state, health,
firmware updates, configuration backups, device settings and fleet-wide commands.

### 1.1 Goals

1. **Safe firmware updates.** They run on the server as a persisted state machine and
   survive restarts. A hard guard refuses any image for the wrong chip family, and 1 MB
   ESP8266 devices are staged through minimal firmware based on free flash, not on the
   model name.
2. **HTTP is enough.** Every feature works without MQTT. MQTT only makes health and live
   state faster.
3. **Usable backups.** The raw `.dmp` is the authority. Decoded JSON is added for diffs
   and drift checks.
4. **First-class SSO.** Native OIDC (code + PKCE, discovery, groups, RP-initiated logout)
   or trusted forward-auth headers, both correct behind reverse proxies, and a full
   standalone mode (local accounts, own TLS) for hosts without proxy or IdP.
5. **Simple to operate.** One static binary, SQLite, one OCI image, config through env
   and file, structured logs, Prometheus metrics. Ready for a Home Assistant add-on.
6. **AI-maintainable.** Small single-purpose packages with fixed interfaces, enforced
   boundaries, a device simulator, and gates that catch regressions (mutation tests on the
   safety-critical code).

### 1.1a Principles (product decisions, 2026-10-19)

1. **Minimal mandatory configuration.** A fresh install starts with almost no
   settings. Defaults: self-signed TLS, local auth with a first-run admin and a first-run
   wizard. The environment holds only bootstrap values (data dir, listeners, and the
   things needed before the database is open). Everything else, SSO/OIDC included, is
   adjusted live in the UI and stored in the database. An environment value still
   locks its field. Legacy environment values are imported into the database once.
2. **Proportionate security for a homelab tool.** Keep the solid basics: CSRF protection,
   authorisation on every route, no secrets in logs or HTML, the chip-family guard, and
   secrets sent only to the address they are bound to. Step-up (re-authentication) only
   for local account management, auth/SSO changes and restore; routine settings don't
   need it. Security reviews: High and Medium findings are fixed; Low and Info findings
   become documented residual risks without further review rounds.
3. **The reference deployment is not a design constraint.** Network specifics of one
   installation (e.g. firewall rules between IoT VLANs and the Home Assistant host) are
   handled in its deployment docs. The Home Assistant add-on (M3) targets other users.

### 1.2 Non-goals

- Automations, schedules, timers, scenes (Home Assistant).
- A switching dashboard beyond a per-device power toggle in the device list (M2).
- Self-update of tasmofleet, passkeys/WebAuthn, open self-registration, an "auth off" mode.
- Self-service registration or password reset by e-mail.
- Building custom Tasmota firmware. Uploading custom builds is in scope for M2 (§8.6).
- Serial flashing / first install of Tasmota.
- Multi-tenant or multi-instance clustering.

### 1.3 Milestones (summary, details in §23)

| M         | Content                                                                                                                                                                                                                                                           |
| --------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| M1 core   | inventory (manual + TasmoAdmin import), health (HTTP poll + optional MQTT), firmware cache + firmware server, OTA engine, backups (raw + decoded), auth (OIDC, header, local accounts, standalone TLS), audit, metrics, image, Unraid template, Grafana dashboard |
| M2 parity | device list with live state + power toggle, device settings editing, console/commands, bulk commands, autoscan (HTTP) + MQTT discovery, policy/drift view, backup diff + restore, custom firmware upload (notifications were planned, not built: §14.5)           |
| M3        | Home Assistant add-on (ingress), move to public GitHub                                                                                                                                                                                                            |

---

## 2. Example deployment

This is the kind of installation tasmofleet is built for. The code stays generic. Concrete
values appear only in `deploy/` examples and in this section (all names and addresses are
placeholders).

| Aspect        | Example                                                                                                                                                                                                                                                                                                                                                 |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Host          | A Docker host with a macvlan/ipvlan network and fixed container IPs in 172.31.12.0/24. An existing TasmoAdmin runs at 172.31.12.60; tasmofleet gets its own fixed IP (172.31.12.61) and can take over .60 when TasmoAdmin is retired (§22)                                                                                                              |
| Container     | Unraid template `deploy/unraid/tasmofleet.xml` (any Docker host works), data directory mounted at `/data`                                                                                                                                                                                                                                               |
| Reverse proxy | Traefik v3, single entrypoint `websecure` on 172.31.12.20. Public name `tasmofleet.example.com`, internal split DNS `tasmofleet.example.internal`. `TRUSTED_PROXIES=172.31.12.20/32`                                                                                                                                                                    |
| SSO           | Authentik at `auth.example.com`, one OAuth2/OIDC provider per app. Issuer `https://auth.example.com/application/o/tasmofleet/`. Groups `Tasmofleet Users` (operator), `Tasmofleet Admins` (admin). Providers created through the API need `grant_types` authorization_code + refresh_token. Redirect URI `https://tasmofleet.example.com/oidc/callback` |
| Devices       | IoT VLANs 172.31.13.0/24 and 172.31.23.0/24, a ~20 device fleet: Sonoff Basic / MagicHome (ESP8285N08), Gosund SP111 / NOUS A1T (ESP8266EX), all ESP8266 with 1 MB flash, plus ESP32-D0WDQ6 lamps (tasmota32). All on 16.0.0, image `release-tasmota` / `release-tasmota32`. Device names `<room>-<thing>` = MQTT topic = hostname. No web passwords    |
| Firewall      | IoT may reach only MQTT (172.31.12.11:1883) and the OTA server, TCP 80. If the rule is IP-based, the firmware server must listen on **plain HTTP port 80 on the container IP** (`FIRMWARE_LISTEN=:80`, `FIRMWARE_BASE_URL=http://172.31.12.61`)                                                                                                         |
| MQTT          | `mqtt.example.internal:1883` (Mosquitto), user `tasmota`. Tasmota discovery retained on `tasmota/discovery/+/config`. TelePeriod 300, FullTopic default `%prefix%/%topic%/`                                                                                                                                                                             |
| Observability | Prometheus scrapes `/metrics`; blackbox probes `/healthz`. A log collector (e.g. Loki via Alloy) reads Docker JSON logs, so tasmofleet logs JSON to stdout. tasmofleet ships `deploy/grafana/tasmofleet.json`                                                                                                                                           |
| Notifications | optional, later: mail, or a Home Assistant webhook (§14.5)                                                                                                                                                                                                                                                                                              |
| Fleet policy  | PowerOnState 0, SetOption0 0 (`save_state`), SaveData 0, Timezone 99 with DST rules, NTP from DHCP. Exception: ESP32 lamps use SetOption0 1. M2 policy view flags drift (§14.4)                                                                                                                                                                         |
| CI            | GitHub Actions on GitHub-hosted runners. Images go to `ghcr.io/hellwand/tasmofleet` and `docker.io/nofuturekid/tasmofleet` (same tags)                                                                                                                                                                                                               |

Example environment for this deployment: `deploy/unraid/tasmofleet.env.example`.

---

## 3. Architecture

### 3.1 Process view

```
                   Traefik (websecure, TLS)            Authentik (OIDC / forward auth)
                          |                                    |
                          v                                    |
   +------------------------------- tasmofleet ---------------------------------+
   |  UI listener :8080  (auth, CSRF, HTMX pages, SSE, /metrics, /healthz)      |
   |      |                                                                     |
   |   web --> inventory, health, ota.Engine, backup, firmware.Cache, audit     |
   |              |            |          |           |          |              |
   |            store (SQLite /data/tasmofleet.db, WAL)        files /data/...  |
   |                           |          |           |                         |
   |                 mqttbus (optional)   tasmota.Client (HTTP, SSRF-guarded)   |
   |                                                                            |
   |  Firmware listener :80 (plain HTTP, GET/HEAD /fw/<release>/<file> only)   |
   +--------------|--------------------------------|----------------------------+
                  | MQTT 1883 (optional)           | HTTP 80 to devices (/cm, /dl, /)
                  v                                v
            Mosquitto  <------- tele/stat -----  Tasmota devices (IoT VLANs)
                                                   | HTTP GET firmware (port 80)
                                                   +-----> firmware listener
```

There are two listeners on purpose. Devices reach only the firmware listener, which has
no authentication, no UI and no state-changing routes. People reach only the UI listener,
through the reverse proxy.

### 3.2 Package map and contracts

Every package has one purpose and can be tested in isolation. The skeleton contains each
interface listed here. Imports point downwards: `model` is the leaf and `app` wires
everything.

| Package                            | Purpose                                                                                                       | Key exported API (contract)                                                                                                                                                                                                                                                                   | Depends on                             |
| ---------------------------------- | ------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------- |
| `model`                            | domain types                                                                                                  | `Device`, `Identity`, `ChipFamily`, `MQTTCapability`, `Health`, `Artifact`, `Job`, `JobState`, `Stage`, `Plan`, `UpdatePolicy`, `Backup`, `AuditEvent`, `Principal`, `Role`                                                                                                                   | stdlib                                 |
| `config`                           | configuration                                                                                                 | `Config`, `Load(environ)`                                                                                                                                                                                                                                                                     | stdlib                                 |
| `clock`                            | testable time                                                                                                 | `Clock{Now, After}`, `Real`                                                                                                                                                                                                                                                                   | stdlib                                 |
| `secretbox`                        | AES-GCM at rest                                                                                               | `Box{Seal, Open}`, `LoadOrCreate(dataDir)`                                                                                                                                                                                                                                                    | stdlib                                 |
| `store`                            | persistence                                                                                                   | `DeviceStore`, `HealthStore`, `JobStore`, `ArtifactStore`, `BackupStore`, `AuditStore`, `SessionStore`, `LocalUserStore`, `Store`, `Open(path, box)`, `Migrations`                                                                                                                            | model, secretbox, modernc.org/sqlite   |
| `tasmota`                          | device HTTP protocol                                                                                          | `Client{Command, DownloadConfig, RootPage}`, `Target`, `TargetFor(Device)`, `ParseStatus2/4/11`, `ParseUpgradeProbe`, `ParseOtaURL`, errors `ErrUnknownCommand`, `ErrAuth`, `ErrUnreachable`, `ErrTimeout`, `ErrForbiddenAddr`                                                                | model                                  |
| `fwselect`                         | **pure** firmware decisions                                                                                   | `Detect`, `FamilyFromHardware`, `Select`, `Decide`, `InspectImage`, `Guard`, `Plan`, `BuildOptions`, `DetectedVariant`, errors `ErrUnknownHardware`, `ErrBuildChoiceNeeded`, `ErrFamilyMismatch`, `ErrFilenameMismatch`, `ErrSafeboot`, `ErrVariantChange`, `ErrNoSuchVariant`, `ErrBadImage` | model                                  |
| `firmware`                         | upstream sources, cache, device-facing server                                                                 | `Cache{Releases, Ensure, URLFor, Prune}`, `Source`, `Server(Cache)`                                                                                                                                                                                                                           | model, fwselect, store                 |
| `ota`                              | update state machine + engine                                                                                 | `Step(job, event, now, cfg)` (pure), `Engine{Run, Submit, Preview, Cancel}`, `DeviceAPI`, `Firmware`, `Backups`, `Notifier`                                                                                                                                                                   | model, fwselect, store, clock          |
| `mqttbus`                          | optional MQTT                                                                                                 | `Bus{Subscribe, Command, Connected}`, `Connect`, `Message`, `Kind`                                                                                                                                                                                                                            | paho                                   |
| `mqttcmd`                          | commands over MQTT (SAFETY)                                                                                   | `Channel{Do, DoLoose, Info, Connected}`, `Doer`, `ErrRefused`                                                                                                                                                                                                                                 | cmdpolicy, mqttbus, tasmota, model     |
| `transport`                        | path choice and routing (SAFETY)                                                                              | `Choose(Facts) Paths` (pure), `Router` (a `tasmota.Client`), `WithPath`, `WithTrace`, `Memory`                                                                                                                                                                                                | cmdpolicy, mqttcmd, tasmota, model     |
| `health`                           | reachability                                                                                                  | `Tracker{Get, All, Run}`, `Options`                                                                                                                                                                                                                                                           | model, mqttbus, tasmota, store, events |
| `events`                           | in-process pub/sub                                                                                            | `Hub{Publish, Subscribe}`, `Event`, `Topic`                                                                                                                                                                                                                                                   | stdlib                                 |
| `inventory`                        | device service                                                                                                | `Service{List, Get, Create, Update, Delete, Upsert}`                                                                                                                                                                                                                                          | model, store, audit, events            |
| `tasmoadmin`                       | import parsers                                                                                                | `ParseDevices(r, key)`, `ParseConfig(r)`                                                                                                                                                                                                                                                      | model                                  |
| `backup`                           | backups                                                                                                       | `Service{Backup, List, Open}`, `Decoder{Version, Decode}`                                                                                                                                                                                                                                     | model, store, tasmota                  |
| `audit`                            | audit trail                                                                                                   | `Recorder{Record}`, `RedactKeys`                                                                                                                                                                                                                                                              | model, store                           |
| `auth`                             | authentication                                                                                                | `Authenticator{Principal, Routes}`, `RoleMapping`, `HashPassword`, `VerifyPassword`, `FromContext`, `WithPrincipal`                                                                                                                                                                           | model, store, httpx, go-oidc, oauth2   |
| `httpx`                            | HTTP plumbing                                                                                                 | `Proxy{Trusted, Middleware}`, `BasePath(ctx)`                                                                                                                                                                                                                                                 | stdlib                                 |
| `web`                              | UI                                                                                                            | `Handler(Deps)`, `OTA`, `Assets`                                                                                                                                                                                                                                                              | interfaces of the above                |
| `metrics`                          | Prometheus text format                                                                                        | `Registry`, `Handler`                                                                                                                                                                                                                                                                         | stdlib                                 |
| `app`                              | wiring, lifecycle                                                                                             | `Run(ctx, cfg)`                                                                                                                                                                                                                                                                               | everything                             |
| `simulator`                        | Tasmota simulator (test only)                                                                                 | `NewFleet`, `Fleet`, `Device`, `DeviceSpec`, `Profile`, `Fault`                                                                                                                                                                                                                               | model, mochi-mqtt (embedded broker)    |
| `testutil/oidcmock`                | mock IdP (test only)                                                                                          | `Provider{Issuer, Handler, NextLogin}`                                                                                                                                                                                                                                                        | stdlib                                 |
| `discovery`, `policy`              | MQTT-first and scan discovery; policy rules and outcomes                                                      | see package docs                                                                                                                                                                                                                                                                              |                                        |
| `cmdpolicy`                        | **pure** classification of free-form commands                                                                 | `Authorize`, `Classify`, `Class`, `PolicyReadable`, `PolicySetText` (console, bulk "Send command", policy rules)                                                                                                                                                                              | model                                  |
| `devicecmd`, `devlock`             | single device commands; one operation at a time per device                                                    | restore, OTA and backup take the per-device lock                                                                                                                                                                                                                                              | see package docs                       |
| `restore`, `settings`, `sysconfig` | restore and replace device; curated device settings; system settings and policy rule versions in the database | see package docs                                                                                                                                                                                                                                                                              |                                        |
| `changelog`, `netaddr`, `web/ui`   | What's new page parser; address formatting; templ components                                                  | see package docs                                                                                                                                                                                                                                                                              |                                        |
| `notify`                           | webhook and SMTP notifications (§14.5, #249)                                                                  | `Dispatcher.Emit`, `Dispatcher.Test`, `Selected`                                                                                                                                                                                                                                              | config                                 |

Boundaries are enforced in `.golangci.yml` (depguard) and listed in `docs/dev/architecture.md`.

### 3.3 Dependencies

Each module needs a reason. Anything not listed needs a spec change first.

| Module                                                                                   | Why                                                                                                                                                                                                                                                                                                                                                                                                                                                                          | Scope         |
| ---------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------- |
| `modernc.org/sqlite`                                                                     | SQLite without CGO, so the binary is static and the image distroless                                                                                                                                                                                                                                                                                                                                                                                                         | prod          |
| `github.com/eclipse/paho.mqtt.golang`                                                    | mature MQTT 3.1.1 client (Tasmota speaks 3.1.1) with auto-reconnect; writing MQTT by hand is not worth the risk                                                                                                                                                                                                                                                                                                                                                              | prod          |
| `github.com/coreos/go-oidc/v3`                                                           | ID token verification, JWKS rotation, discovery; security-critical, do not hand-roll                                                                                                                                                                                                                                                                                                                                                                                         | prod          |
| `golang.org/x/oauth2`                                                                    | authorization code exchange with PKCE (`oauth2.S256ChallengeOption`)                                                                                                                                                                                                                                                                                                                                                                                                         | prod          |
| `github.com/mochi-mqtt/server/v2`                                                        | embedded MQTT broker for tests and `tasmofleet-devenv` only (depguard keeps it out of prod packages)                                                                                                                                                                                                                                                                                                                                                                         | test          |
| `github.com/a-h/templ`                                                                   | typed HTML components for the web UI (design v2 port, plan section 4): template errors become compile errors, props are typed. The runtime is a small package; the generator is pinned in `tools/go.mod` and its output (`*_templ.go`) is committed, so the build and the Dockerfile need no generator (`make templ`, `make templ-check`). templ imports only in `internal/web` and `internal/web/ui` (depguard); `templ.Raw`, script and css components are forbidden (CSP) | prod          |
| htmx 2.0.11 + htmx-ext-sse 2.2.4                                                         | vendored JS files, no npm in the build                                                                                                                                                                                                                                                                                                                                                                                                                                       | prod (static) |
| `@playwright/test` 1.65.0                                                                | e2e only, in `e2e/`                                                                                                                                                                                                                                                                                                                                                                                                                                                          | test          |
| `pixelmatch` 8.0.0, `pngjs` 7.0.0 (+ `@types/pngjs`)                                     | pixel comparison against the design prototype (`make visual`); pixelmatch ships its own types                                                                                                                                                                                                                                                                                                                                                                                | test          |
| tools module (`tools/go.mod`): staticcheck, golangci-lint v2.15.0, govulncheck, gremlins | gates, pinned, isolated from the main module graph                                                                                                                                                                                                                                                                                                                                                                                                                           | dev           |

Deliberately not used: `prometheus/client_golang` (counters and gauges are about 150 lines; it
would add ~15 modules), web frameworks/routers (Go 1.22+ `ServeMux` patterns are enough),
ORMs and migration libraries (about 60 lines of migrator), config libraries, CLI frameworks.
Stdlib features used: `net/http.CrossOriginProtection` (CSRF), `crypto/pbkdf2` (password
hashing), `log/slog`, `embed`, `html/template` (its escaping helpers and `template.HTML` for markup a component already escaped; the pages are templ),
`crypto/ecdh`, `crypto/ecdsa`, `crypto/hkdf` and AES-GCM for Web Push (14.5, #610): the VAPID token (RFC 8292) and the
payload encryption (RFC 8291) are about 200 lines held to the RFC 8291 test vector, so no Web Push module is added.

---

## 4. Data model and migrations

SQLite file `<DATA_DIR>/tasmofleet.db`. Opened with `journal_mode=WAL`,
`foreign_keys=ON`, `busy_timeout=5000`, `synchronous=NORMAL`, and **one** open connection
(`SetMaxOpenConns(1)`). The fleet has tens of devices, so a single writer removes
`SQLITE_BUSY` handling entirely.

Migrations are plain SQL files in `internal/store/migrations/NNNN_name.sql`, embedded
and applied in order inside one transaction each. `PRAGMA user_version` stores the last
applied number. A merged migration file is never edited. Downgrades are not supported:
the store refuses to open a database whose `user_version` is newer than the binary
knows, and says so. Before applying migrations, the store writes a copy of the database
to `<DATA_DIR>/tasmofleet.db.pre-<N>` (only when migrations are pending; the newest 3
copies are kept).

Tables (35 migrations, `0001` to `0035`; the files in `internal/store/migrations/` are authoritative, this list names what each table is for):

| Table                                                | Content                                                                                 | Notes                                                                                                                                                                             |
| ---------------------------------------------------- | --------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `devices`                                            | inventory + detected identity + coarse health                                           | unique MAC (if known), unique topic (if set), unique (address, port). `http_password` sealed with secretbox. `ota_excluded`, `mqtt_capability`                                    |
| `health_events`                                      | health state transitions                                                                | written on change only, pruned after 90 days (fixed)                                                                                                                              |
| `device_boots`, `device_wifi_samples`                | restarts TasmoFleet noticed; hourly Wi-Fi signal                                        | migration 0041 (#325). Newest 50 restarts per device; samples pruned after 7 days                                                                                                 |
| `firmware_artifacts`                                 | cache index                                                                             | unique (release, filename), sha256, family proven by image header, `safeboot` flag                                                                                                |
| `ota_batches`, `ota_jobs`, `ota_job_events`          | OTA                                                                                     | partial unique index: at most one non-terminal job per device. Every transition writes a row to `ota_job_events`                                                                  |
| `backups`                                            | backup index                                                                            | files under `<DATA_DIR>/backups/<dir>/` (`<dir>` is the sanitised hostname, see below; every row stores its own relative path); `kept` pins a run, `trigger` says what started it |
| `audit_log`                                          | append-only audit                                                                       | pruned by `AUDIT_RETENTION` (default 1 year); the same age also prunes old jobs and failed backup attempts                                                                        |
| `sessions`                                           | web sessions                                                                            | the store holds `sha256(cookie)`, never the cookie                                                                                                                                |
| `device_cookies`                                     | remembered browsers for the login rate limit                                            | hashed id, bound to the password generation (§15.1)                                                                                                                               |
| `local_users`                                        | local accounts (role, disabled, must_change_password)                                   | written by CLI and admin UI with step-up                                                                                                                                          |
| `settings`                                           | small key/value (e.g. last firmware index refresh)                                      | not for configuration                                                                                                                                                             |
| `system_settings`, `policy_rule_versions`            | operational settings (§16) and every saved version of the policy rules                  | sealed secrets, append-only versions                                                                                                                                              |
| `principals`, `user_prefs`                           | SSO and proxy users seen at sign-in; per-user UI preferences                            | roles of principals are a record of the last sign-in                                                                                                                              |
| `discovery_proposals`, `device_macs`                 | discovered devices waiting for an admin (expire after 30 days); the MAC set of a device |                                                                                                                                                                                   |
| `restore_jobs`, `firmware_custom`, `backup_attempts` | restores; provenance of uploaded firmware; failed backups                               |                                                                                                                                                                                   |
| `id_high_water`                                      | last id handed out for `devices` and `ota_batches`                                      | migration 0047 (#498): an id is never handed out twice, because the audit log, links and notifications name devices and transactions by id                                        |
| `push_vapid`, `push_subscriptions`                   | the Web Push VAPID key; one row per subscribed browser                                  | migration 0049 (#610): the private key sealed with secretbox; a subscription is owned by the admin who made it                                                                    |

Timestamps are INTEGER unix milliseconds, UTC. JSON columns (`tags`, `identity_before`,
`principal`, `detail`) are plain TEXT, encoded and decoded in `store`.

File layout under `/data`:

```
/data/tasmofleet.db (+ -wal, -shm)
/data/secret.key                 32 random bytes, 0600 (device passwords at rest)
/data/firmware/<release>/<file>  cache, 0644 (served to devices; contains no secrets)
/data/backups/<dir>/<UTC stamp>_<hostname>.dmp               0600
/data/backups/<dir>/<UTC stamp>_<hostname>.snapshot.json
/data/backups/<dir>/<UTC stamp>_<hostname>.decoded.json
/data/tmp/                       decoder scratch (PyInstaller extraction), cleaned on start
/data/tasmofleet.env             optional config file
/data/csrf.key                   32 random bytes, 0600 (CSRF, device cookies, session signing)
/data/tls/                       generated self-signed certificate (§15.3a)
/data/initial-admin-password     only until the first admin has changed it (§15.1)
```

Backup naming (0.18): `<hostname>` is the device's current hostname (Status 5) reduced to
`[A-Za-z0-9._-]` (other characters become `_`, leading dots dropped, at most 63 bytes), else the
sanitised inventory name, else `device-<id>`. A directory that already holds another device's
or a deleted device's backups (case-insensitive, decided from the `backups` index under one
manager-wide lock from the choice to the first indexed file) is not shared: `<hostname>_<id>` is
used. A hostname change only affects new backups; old files are never moved, the index finds them
by path. Files of 0.17 (`backups/<device-id>/<stamp>-<kind>.<ext>`) are moved at every start by an
idempotent migration: SHA-256 of the file must equal the row, `os.Rename` on the same file system,
then a compare-and-set update of the row path. A crash between rename and update is repaired at the
next start; anything unclear (checksum mismatch, both or neither file, target used by another row)
is left untouched and logged; nothing is deleted except empty numeric directories (`os.Remove`).
Backups of deleted devices stay where they are. No SQL migration; a downgrade to 0.17 still reads
the moved files because it reads by the stored path.

All files are created with explicit modes. The process never runs `chmod -R` over the
data directory (TasmoAdmin lesson A4).

---

## 5. Device protocol facts (package `tasmota`)

- `GET http://<addr>:<port>/cm?cmnd=<urlencoded>` always answers 200 with
  `application/json`, chunked, `Connection: close`, one JSON object (OBS). Unknown
  command: `{"Command":"Unknown","Input":"<UPPER>"}`. Known command without output:
  `{"Command":"Error",...}`. Empty: `{"WARNING":"Enter command cmnd="}`. Wrong
  credentials: 401 `{"WARNING":"Need user=<username>&password=<password>"}` (SRC).
- Credentials are sent as `Authorization: Basic`, never as `user=`/`password=` query
  parameters (those end up in logs and error messages, TasmoAdmin lesson B2). Every
  error and log line passes through a redactor.
- The client allows **one request at a time per device** (ESP8266 web servers are
  single-threaded). Requests to the same device queue, requests to different devices run
  in parallel up to a global limit (default 8).
- Timeout classes: `Status N` 10 s, `OtaUrl`/`Upgrade` 15 s, `/dl` 20 s, `GET /` 10 s.
  Responses over 1 MiB are truncated and rejected.
- SSRF: `Target` can only be built from an inventory `Device`. The dialer resolves the
  host and refuses any IP outside `DEVICE_NETWORKS`. The check runs on the resolved
  address at connect time (`net.Dialer.Control`), so DNS rebinding does not bypass it.
  Redirects are not followed.
- **`Status 0` is never sent by automated code.** It returns 12 sections at once; on ESP8266
  with <20 kB heap this produced spliced, undecodable payloads in this fleet. Identity
  probing uses `Status 1`, `Status 2`, `Status 4`, `Status 5`, `Status 6`, `Status 11` and plain `Status` (the
  Status object only: DeviceName and the resolved Topic), one at a time, ≥200 ms apart.
- Minimal firmware (SRC): only `Upgrade`, `Upload`, `OtaUrl`, `SerialLog`, `Restart`
  exist; everything else is Unknown. The running version comes from bare `Upgrade`
  (`{"Upgrade":"1 or >16.0.0 to upgrade"}`). The image name comes from the INFO1 both keep
  publishing on the first MQTT connection after boot (`"Version":"16.0.0(release-minimal)"`) or from
  `GET /`: footer `Tasmota 16.0.0 (release-minimal) by Theo Arends`, `<html lang="..">`,
  and the heading `MINIMAL firmware<br>please upgrade` (`RootPage`). On ESP32 the
  safeboot partition shows a "Safeboot" page with the same command table.
- **Minimal detection rule** (#686): a device runs minimal (or ESP32 safeboot) only if
  `Status 2` answers `{"Command":"Unknown"}` **and** one of these names the image, in this order:
  (1) the INFO1 the device sent since its last boot, read through the router for a stored device
  usable over MQTT (`transport.Router.Info1Root`, a passive read of the broker; its version must
  match the bare `Upgrade` reply), inside TasmoFleet's own update job only when the job is pinned to
  MQTT: the INFO1 comes from the topic, while an HTTP job's `Status 2`, `Upgrade` and upload go to
  the address, and a check counts only for the transport it was read over (#810); (2) inside such a
  job pinned to HTTP, or pinned to MQTT without such an INFO1 (MQTT down, no INFO1 since the boot),
  the main page at the job's address, over the path the job pinned (#918; over MQTT that is the
  INFO1 again, so the step fails); (3) outside such a job, without an INFO1 (a device found
  stuck in minimal, the first add, the preflight of a device that already runs minimal): `GET /`
  with the minimal/safeboot heading or the image tag `release-minimal`/`release-safeboot` (a self-compiled build's bare `safeboot` counts, a bare `minimal` does not: §6.2, #584). Inside a job
  (`fwselect.Inputs.JobFamily`, the chip family the job read at its preflight) the image, from the
  INFO1 or the page, must be the intermediate image of that family (minimal on an ESP8266,
  safeboot on the ESP32 line), otherwise `ErrFamilyMismatch`. Minimal has no `Status 5`, and
  `fwselect.Guard` checks the job's preflight identity, not the one the device answers with during
  the job (`hardwareChanged` ignores the unknown family of safeboot), so this image is the only
  proof that another chip has not taken the job's address during a restart. The bare `Upgrade` reply
  (`1 or >X to upgrade`) is identical on normal firmware (OBS, all fixtures). It only
  supplies the version (`tasmota.UpgradeProbe.Idle`, `fwselect.UpgradeProbe.Version`)
  and never proves minimal on its own.
- **Identity source (#87, #682).** The probe is the primary source (`Prober.probe`; docs/security/mqtt-transport.md T20).
  For a stored device that the router reads over MQTT (Choose) and that has a stored MAC and topic it goes MQTT
  first, also while HTTP works: `Status 5` on the stored topic, where every reply within 1.5 s is heard
  (`Router.TopicStatus5`), and only exactly one reply with a stored MAC lets it go on; `Status 2`, `Status 4` (the
  language is the build's locale ID, #686) and the facts follow, pinned to MQTT, and the store writes the result only
  while the device still has the topic that was asked (`store.ProbeGuard.Topic`). A second reply or another MAC marks
  the topic (#852) and, like a check that learnt nothing or a read after it that goes unanswered, sends the whole probe
  to the address, whose `Status 5` vouches for its `Status 2`, `Status 4` or bare `Upgrade` (and on minimal firmware
  the image source of the minimal detection rule). An address that is no stored device yet (no topic, no MAC to
  compare), minimal and safeboot firmware (no `Status 5`) and a device MQTT cannot serve are probed at the address.
  Threat model (#682): a client with write access to the device's command topic can send `OtaUrl` and `Upgrade` to it
  anyway, so a forged identity gives it nothing new; the broker ACL protects against it, the check against another
  device on the same topic. For a device on our broker
  (`mqtt_capability = ours`) whose identity no probe has stored (`identity_at` unset: not reachable, refused, or
  not yet tried) MQTT is a second source, one that never replaces the
  first: (1) `tele/<topic>/INFO1`, published at every boot, carries `Version`; it is read with
  the `StatusFWR.Version` grammar (`fwselect.DetectInfo1`). INFO1 has no hardware, so on its own
  it fills the version only (never image or variant, and nothing once the hardware is known) and
  the identity is not `Known()`. (2) Once a device has
  been online for 30 s (HTTP gets the first go) and still lacks its hardware, tasmofleet asks
  `Status 2` over MQTT (`cmnd/<topic>/STATUS` with payload `2`, through the configured
  FullTopic) and reads `stat/<topic>/STATUS2` through `fwselect.Detect`, so family, hardware
  and the `Known()` rule are the same as over HTTP. The question follows discovery's Status 5
  limits: only online devices, once per device per 10 minutes, at most one per second on the
  bus, one at a time per device, never `Status 0`, logged and audited (`identity.mqtt_ask`);
  minimal and safeboot firmware (no Status 2) are not asked, and an answer claiming such an
  image is refused. Only devices recorded as `ours` are read (an INFO1 seen earlier is kept
  until then); the online state is dropped when the broker connection is lost, so the grace
  starts anew after a reconnect. Only a reply within 30 s of the
  question is taken: anyone on the broker can publish a STATUS2. MQTT writes only the six
  identity fields (family, hardware, single core, version, image, variant) and only while
  `identity_at` is unset (`store.UpdateIdentityFromMQTT`, compare-and-set); this reader never sets
  `identity_at`, so the first probe still runs and its identity replaces the reader's, and
  a probe that finishes meanwhile always wins. OTA is unchanged: the engine reads the identity
  before it plans and `Guard` runs on that identity. A retained INFO1 may describe the last
  boot only; a device on a stale one is corrected by its next boot or its next probe.
- **Refresh identity (#91, #65).** `Prober.Refresh(ctx, id, actor)` re-reads and stores every
  identity field (where each read goes, #682: over MQTT first after the check that hears every reply on the topic, as
  in "Identity source" above, else at the address: `Status 5` without credentials first, `Status 2`, `Status 4` (or the
  bare `Upgrade` and the image source of the minimal detection rule), then `Status 6`, `SetOption3`, `Status 1` and plain
  `Status`; docs/security/mqtt-transport.md T20 and "What still uses HTTP"): firmware, build, chip, hardware, language and flash (Status 2/4), the network
  facts of Status 5 (hostname, IP, gateway, subnet mask, DNS servers; Wi-Fi from Status 11 follows
  in the background), MQTT broker (Status 6, SetOption3), the OTA URL (one read-only `Status 1`, best
  effort, never the `OtaUrl` command; credentials in the URL are stripped before it is stored, a value
  that cannot be parsed is dropped) and DeviceName, resolved topic and relay names (the FriendlyName
  array) from plain `Status`. Subnet, DNS, OTA URL and relay names are stored on the device
  (migration 0031: `net_subnet`, `net_dns`, `ota_url`, `relay_names`); minimal firmware and a failed
  `Status 1` keep the stored values. The group topic (`GroupTopic` of the same `Status 1` answer, or of
  the INFO1 boot announcement, normalised from `cmnd/<group>/` to `<group>`) and the times `ota_url` and
  `group_topic` were read are stored too (migration 0035: `group_topic`, `ota_url_read_at`,
  `group_topic_read_at`, the stamps NULL until read); the device list shows both as optional columns, read-only,
  and never reads a device or a backup file to fill them (#239). GroupTopic2 to GroupTopic4 come from the bare
  `GroupTopic` (a read: Tasmota writes only with an argument, and answers all four keys `GroupTopic1` to `GroupTopic4`
  for every form), which the probe sends after `Status 1`; only an answer with all four usable keys is stored. The
  non-empty ones are stored as a list next to `group_topic` with their own read stamp (migration 0054:
  `more_group_topics`, a JSON array like `relay_names`, and `more_group_topics_read_at`, NULL until read; no backfill),
  in `model.Device` as `MoreGroupTopics` and `MoreGroupTopicsReadAt` (#1005). GroupTopic1 stays in `group_topic`, which
  INFO1 and `Status 1` keep writing alone; they never touch the list or its stamp. `Device.GroupTopics()` (GroupTopic1
  first, then the list) and `Device.GroupTopicsReadAt()` (the older of the two stamps, zero when either is unread) are
  the one view of all four. A probe stores the list with the stamp of its `Status 1`, and one whose `GroupTopic` read
  failed leaves the list older than the probe, so it counts as unread. When TasmoFleet itself may have changed the list
  (a `GroupTopic<n>` write, a restore upload), `ForgetGroupTopics` clears its stamp and counts up
  `more_group_topics_gen` (`model.Device.MoreGroupTopicsGen`); the probe and the reader pass the count they saw when
  their read began (`store.ProbeGuard.MoreGroupTopicsGen`) and the store keeps their answer only while it is unchanged,
  so an answer older than the change never looks fresh. The device list's Group topic column shows the
  first with "+N" for the others and all of them in the title, the list filter finds a device by any of them, and the
  Identity card lists them all.
  _MAC first:_ the probe sends `Status 5` first, without credentials, and compares the MAC
  (Wi-Fi or Ethernet) with the stored set before any other command. A device that answers it
  without credentials has no web password, and the whole probe runs without them; only a 401
  repeats `Status 5` with the stored credentials. Another MAC refuses the refresh
  (`app.RefreshRefusedError`, audit `device.refresh_refused`, shown on the device page; nothing
  written, nothing more sent). A device stored without a MAC gets the reported one bound
  (audit `device.mac_bound`) when a person refreshes it (`actor` set; operators may refresh, so
  an operator's refresh binds a missing MAC too, accepted 2026-10-26); automatic refreshes
  (discovery accept/reprobe, `actor` "") never bind (discovery D8). Only a valid, non-zero MAC
  (`inventory.NormaliseMAC`) is bound. A MAC another device holds is refused the same way
  (`store.ErrMACInUse`). Minimal firmware has no Status 5: a device with a stored MAC is then
  refused (`refusedUnverified`, the stored identity and `identity_at` stay); one without a MAC
  is refreshed. _Name and topic:_ the store records the DeviceName and topic of every probe
  (`device_name`, `device_topic`, migration 0029). The stored name follows the DeviceName while
  it is still the recorded one, or, for devices probed before 0029, while it is the hostname,
  topic or address tasmofleet filled in itself (not when the DeviceName is Tasmota's default
  `Tasmota`). A name the user chose differs from these and is kept (#52); the Identity card then
  shows the notice "Name on the device: X" (#125) while `device_name` differs from `name`, is not
  Tasmota's default `Tasmota` and is not equal to `device_name_kept`. Admins can answer it: "Use device
  name" sets the name through `Inventory.UpdateIf` (compare-and-set, audited as `device.update`), "Keep"
  stores the declined name in `device_name_kept` (migration 0030, audited as `device.name_kept`), so the
  notice returns only when the device reports another name. An answer for a name the device no longer
  reports is refused. Operators see the notice without the buttons. The Identity card's first row is the
  TasmoFleet ID (the device's primary key, with a copy button): the number the log names the device by (#1004). The stored topic is
  only a mirror used to match MQTT messages, so it always follows the topic the device reports
  at the address (the probe's reads over MQTT keep it: a device answers only on its own topic, #851;
  #111; also for rows from before migration 0029, whose `device_topic` is empty), audited as
  `device.topic_changed` with old and new. If another device already has that topic, the
  stored one is kept, `device.topic_kept` is audited and the device page says so after the
  refresh (`TopicKeptError`). The retained LWT of the new topic is applied at once by the
  health reload (#105); what health learned on the old topic (its LWT, often the Offline Tasmota
  sends there on a topic change) is dropped first (#119). _Transport (#682):_ a device the router reads over MQTT
  is refreshed over MQTT first, as in "Identity source" above. The MAC rule holds there too: only exactly one reply
  with a stored MAC to the check that hears every reply lets the refresh read on; a second reply or another MAC marks
  the topic (#852) and the refresh goes to the address, so with HTTP failing it fails and writes nothing. The language
  is the `Status 4` locale ID, `identity_at` is set as at the address, no MAC is bound (a device without a stored MAC
  never takes this path) and the topic the device reports is not taken (mqtt-ingest.md R6). A refresh a person or
  discovery started that went over MQTT is audited (`identity.mqtt_ask` with the actor, the topic and `via` `mqtt`);
  the background refresh is not. The DeviceName read there follows the rule of the address; this is accepted and
  cosmetic: anyone with broker access can change the name of a device whose name was never edited by hand; a
  hand-set name only gets the notice below. Until 0.39.0 a separate refresh over MQTT ran only when HTTP failed, heard
  one reply, read no `Status 4` and wrote the identity only while `identity_at` was unset (#91); the probe replaces it.
  Contract change in this step: `web.Prober.Refresh` takes the actor; `store.ProbeResult` gained
  `DeviceName`, `NameFrom`, `DeviceTopic`, `BindMACs` and lost `Topic` (the old repair rule is
  part of the topic rule above).
- Minimal firmware never persists settings (SRC: `SettingsSave` is compiled out). An
  `OtaUrl` set while minimal runs lives in RAM only.
- Command output over HTTP depends on `WebLog >= 2`. If a reply is a `WARNING` about the
  web log level, the client sets `WebLog 2` once, logs it (flash write!), and retries.
- **Push upload (F-9, optional per-transaction delivery, ESP32 only; SRC `xdrv_01_9_webserver.ino`)**:
  the web UI's "Upgrade by file upload" is `GET /up` then a multipart `POST /u2?fsz=<size>`
  (field `u2`, non-empty file name, first byte `0xE9` or `0x1F` for gzip, exact
  `Content-Length`, no chunking). The upload type is set by the `GET /up`, not by the
  POST: a POST without it answers "Upload Successful" and restarts the device without
  flashing. `Client` therefore fetches `/up` first, requires the upload form
  (`action='u2?fsz='`) and holds the device slot for both requests (`tasmota.Uploader`).
  The reply page says `Successful` or `Failed` plus a reason; a refusal (or a missing
  form) is certain, nothing was flashed, and the job fails at once instead of waiting for a
  reboot. Any other error after the body started is not certain and is probed. The image
  must fit Status 4 `Free` less one 4 KiB sector (Tasmota's `Update.begin` bound); an
  ESP32 with the safeboot layout (one app partition) reports only the rest of its running
  partition there and cannot take a full image while running (research:
  `tasmota-minimal-and-ota.md`, "File upload on an ESP32 with the safeboot layout"). The state machine is unchanged: `set_ota_url` only runs Guard,
  `upgrade_issued` uploads, a lost reply is probed and never resent, `OtaUrl` is never
  touched. The choice is "push where possible": the download stays the default for every
  device, and with push chosen each device that cannot take an upload (ESP8266, unknown
  chips, safeboot/minimal, staged plans, an image larger than the free flash or an
  unknown free flash) gets the download instead. The preview says per
  device which delivery it gets and why, the job stores the delivery it really uses
  (shown on the transaction page), and a job submitted as a push whose plan no longer
  allows one fails before touching the device instead of switching unseen. A push job
  sends its image once (no second attempt after a stalled or lost upload). ESP8266 stays
  on the download until a real-hardware test.
- **Push into safeboot (#30, ESP32 with the safeboot layout; gate `safebootPushEnabled`, open since the
  real-device test)**: an image larger than the running partition's free flash goes
  through the factory partition, as the web UI does. `GET /up` tells the layout without side effects:
  the button calls `upl(this)` only in the single-OTA form (`tasmota.ParseUpLayout`, research doc).
  The job (`Job.ViaSafeboot`, decided in the preview, `PreviewItem.ViaSafeboot`) runs
  `set_ota_url` (Guard only) -> `safeboot_switch` (Guard, **one** `GET /u4?u4=fct&api=`; `false`
  means restarting; probe until the device answers as safeboot, `SafebootWindow`) -> `upgrade_issued`
  (Guard again, `/up` must now show the plain form, one upload to `/u2`) -> `await_reboot` -> `verify`.
  `/u4` is never re-sent (the browser script re-sends it every 6 s; tasmofleet does not) and a lost
  reply is probed. A job that ends with the device in or possibly in safeboot (upload refused or lost,
  deadline in safeboot, Guard refusal, cancel) goes through `safeboot_recover`: probe first, **one**
  `GET /u4?u4=ota&api=` on the first answer from safeboot (an app answering needs nothing, a device
  still offline would swallow it; `Job.RecoveryStep` is stored before the request and a second
  recovery after a failed verify is refused); the old version answering ends the job `failed` ("Upload into safeboot
  failed; the device was switched back to <old>"), anything else ends it `failed` with the address
  and the manual instruction. A restart of tasmofleet resumes both states by probing only. The image
  must start with `0xE9` (checked before the device is restarted). With the gate closed the preview says
  "Downloads instead: push into safeboot is not enabled yet". `await_reboot` may be skipped: when the first
  probe after the upload already sees the new app (uptime below the reboot proof), the job goes straight to `verify`.
- Device output is untrusted text. It is size-limited and parsed with tolerant JSON
  (control characters stripped), and HTML-escaped by templ on output.

### 5.x Transport and path choice (package `transport`, `Choose`)

`transport.Choose(Facts) Paths` decides how each action reaches a device. It is pure
(`internal/transport/choose*.go`, depguard, mutation-gated) and returns one `Path` per action
(`HTTP | MQTT | DownloadHTTP | DownloadMQTT | PushHTTP | PushSafeboot | PushMQTT | None`) with one
English sentence each (`Paths.Reasons`), shown verbatim in the UI and in errors. `Facts` come from
a `FactsSource` (`internal/app/transportfacts.go`): HTTP state (ok = success within 15 min; failing =
the router remembers failed calls since a time and a kind, cleared by a later success; else unknown),
MQTT usability, the MQTT password state, `FWReach` ("no" older than 24 h counts as unknown),
whether FIRMWARE_BASE_URL is set, the identity, and for an update the request (Delivery,
AllowMQTTUpload, the `fwselect.MQTTUploadFits` verdict, the HTTP push decision the OTA engine makes).

MQTT is usable when all hold: the bus is connected, `Device.MQTT` is ours, the topic is one device's
topic (non-empty, no `/ + #`, no whitespace, not `tasmotas`), no device in the inventory listens on it
(`TopicExclusive`), the FullTopic equals MQTT_FULLTOPIC (empty = `%prefix%/%topic%/`, trailing `/`
ignored), the topic is not the device's own GroupTopic (even a stale value blocks), and the last will says
Online. `mqttcmd` checks the topic again before every publish.

Exclusivity is decided on command stems, expanded as Tasmota's `GetTopic_P` does with the bus's real command
prefix (mqttbus `Prefixes[0]`): `%prefix%`, `%topic%`, `//` collapsed, `/%prefix%` appended when missing,
trailing `/` dropped. Tasmota subscribes to stem + `/#`, so two stems collide when they are equal or one is a
level prefix of the other (`cmnd/a` and `cmnd/a/b`; not `cmnd/a` and `cmnd/ab`). The stem of the device's
Topic and of every device's group topics, GroupTopic1 to GroupTopic4 (fresh or stale, the device's own included, #1005), are compared with it, plus
the raw topic strings. A FullTopic without `%topic%` or with another placeholder (`%hostname%`, `%id%`)
has no knowable stem: for this device it means not exclusive, and so does such a FullTopic on any other device
(unknown is not exclusive). Devices with neither Topic nor FullTopic are HTTP only and ignored.

Limits: devices outside the inventory on the same broker are invisible and can still make a message reach more
than one device. GroupTopic2 to GroupTopic4 are read with GroupTopic1 since #1005.

The group topics are trusted as "known" only when GroupTopic1 is set and both stamps, GroupTopic1's and GroupTopic2 to
4's (`Device.GroupTopicsReadAt`), are set and not older than the last Status 1 read (they can be stale: INFO1 is sent
at boot and carries GroupTopic1 only, Status 1 and the bare GroupTopic only when probed). A read and empty
GroupTopic2 to 4 list is known; one never read (every device right after the upgrade to 0.42) is unread, under the
same rules as an unread GroupTopic1. TasmoFleet's own `GroupTopic<n>` with a value that may have reached a device
(console, bulk command, update job; any part of a Backlog, `cmdpolicy.SetsGroupTopic`) and a restore upload that may
have reached it clear `more_group_topics_read_at` and keep the values (`store.ForgetGroupTopics`): the list is unread
until the reader has asked the device again. A restore asks it itself once the device is back, before the verifying
backup, which the unread list would otherwise keep off MQTT. Commands are reads (Status N,
identity refresh) and tolerate an unknown own GroupTopic; Writes (settings, Make compliant, console
commands that change the device, backlog), transfers and the download need a known one and also that no
other device with a topic has an unread GroupTopic (empty, never stamped, or older than that device's last probe; a device whose last will is Offline is left out, since our commands are never retained, while its topic still counts for exclusivity). A GroupTopic is free text, so an unread one could equal any device's topic: no device is left out by comparing topics. Left out as well is a device provably not on our broker (#812): its last Status 6 put it on another broker or none, no device stored as ours names its broker host and port and the broker matcher with its aliases does not accept them, and nothing from its topic was ever heard on ours although its last will would be (its FullTopic is MQTT_FULLTOPIC, its topic one level) and the bus has been subscribed for 30 s, long enough for the broker to replay the retained wills after a start, a reconnect or a broker change. Silence counts only while the bus (its will ledger, `mqttbus.WillLedger`, reset on every connect) has heard at least one will on its current connection, so a broker that lets TasmoFleet publish but not read exempts nobody, and has no device up under a topic no stored device has (a will that is not Offline, or live telemetry or a live reply, brings a topic up; only an Offline will takes it down, a cleared will keeps its last state), so a device that moved to our broker with a new topic cannot stay exempt (#972); an unknown topic whose latest will is Offline does not count (Tasmota leaves one on its old topic at every topic change, and an Offline device cannot receive a command), and a ledger past its 4096 topics exempts nobody. The reason names the devices that hold the write back, three at most and the rest counted: "the group topics of A, B and C have not been read yet, so a message to this device's topic could reach them; refresh their identity". Reads are unaffected. The reason text says "refresh the
device's identity".

The consumer (the router) computes Facts and calls Choose from the same device snapshot it sends with, and
re-checks the topic at send time (`mqttcmd`), so an edit between the decision and the publish cannot redirect
a message.

Order (reads MQTT first since #456, writes since #605, backups and restores since #607, the commands of an update
since #608; the upload of a push stays HTTP while HTTP works). HTTP "not failing" means ok or unknown.

| Action                      | Rule                                                                                                                                                                                                                                                                                           |
| --------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Commands (reads)            | MQTT when MQTT is usable (on our broker, topic exclusive, FullTopic matches, last will Online; an unknown own GroupTopic is fine), whatever HTTP's state, except that a device on whose topic another device answered a MAC check (`TopicTakenSince`, #852, until the topic is its own again), whose MQTT read or write just went unanswered (`ReadsUnansweredSince`, 10 min) or whose MQTT connection the router just changed (`ConnChangedSince`, 6.6) is read over HTTP while HTTP is not failing; else HTTP when HTTP is not failing; else None. The router sends a read over HTTP once after MQTT when nothing was published, or when it was published and not answered (timeout) while the caller still waits and HTTP is not failing (that marks the device); never back to MQTT within a call, never a second HTTP request. State, a bare Gpio, Gpios and Modules stay HTTP first (their replies do not match the channel's rules). |
| Writes                      | MQTT when MQTT is usable with a known own GroupTopic, no other device's unread and no other device found answering on its topic (the strict rule, #852), whatever HTTP's state, except that a device whose MQTT read or write just went unanswered or whose MQTT connection was just changed is changed over HTTP while HTTP is not failing; else HTTP when HTTP is not failing; else None. What a write carries (a secret, a change of the MQTT connection, a template) is no reason for HTTP (6.6), and Make compliant follows the same rule (#606). A write MQTT did not publish goes over HTTP once; one MQTT published and nobody answered is an unknown outcome, never resent (it marks the device like an unanswered read). The power toggle asks the same rule (#605). |
| Backup                      | MQTT FileDownload when MQTT is usable under the strict rule with a password that is set and not refused, whatever HTTP's state (#607), with the two exceptions of writes (MQTT commands just unanswered, MQTT connection just changed: HTTP while HTTP is not failing); else HTTP `/dl` when HTTP is not failing; else None. The router sends the backup over HTTP once when the FileDownload published nothing; one that published anything never goes on over HTTP. The files archive follows a dump over MQTT where the firmware sends files over MQTT (`FileDownload` Type 8, #924) and the device has no SD card, else it goes over HTTP (12.1). A device that only lacks a usable MQTT password for the FileDownload gets no `/dl` while HTTP does not answer (failing, or silent to one `Status 11` the router asks when no fresh answer is on record): the backup ends at once with "needs an MQTT password" (#997). |
| Restore                     | MQTT FileUpload Type 2 under the backup's rule (#607); else the HTTP restore upload (`GET /rs` + `POST /u2`) when HTTP is not failing; else None (#430: the firmware server plays no part). The files of the backup go over HTTP either way (12.4). |
| Update, pull                | The device downloads the image from TasmoFleet's firmware server over HTTP either way (base URL set, FWReach not "no"). OtaUrl and Upgrade go over MQTT (DownloadMQTT) under the rule of writes, whatever HTTP's state, with the same two exceptions (MQTT commands just unanswered, MQTT connection just changed: DownloadHTTP while HTTP is not failing) (#608); else DownloadHTTP when HTTP is not failing; else None. No MQTT password is needed. The job pins the path for its whole life (D5): a lost answer is probed over the same path, never resent over the other. |
| Update, push where possible | HTTP not failing: the engine's decision (PushHTTP, PushSafeboot, or its fallback to the download, which is a pull and follows the row above, kept with its note). HTTP failing and MQTT usable: PushMQTT when the per-transaction "experimental" checkbox is set, the image fits and the password is usable; else DownloadMQTT; else None |

The one sentence of an update path is `Path.Describe` and names both legs: the transport of the commands (OtaUrl, Upgrade) and how the image reaches the device, which crosses the broker only for the experimental PushMQTT (refused today) (DownloadHTTP and DownloadMQTT: the device fetches it over HTTP from TasmoFleet's firmware server; PushHTTP and PushSafeboot: TasmoFleet pushes it over HTTP; PushMQTT: over MQTT, experimental). Every update `Reason` starts with it and only adds why; the device page, the update preview and the job log's start line all show that sentence (#987), so a path reads the same everywhere.

Both download paths also need FIRMWARE_BASE_URL (the device needs an address to fetch from). A password "not set", "refused since T" or in broker mode without a configured MQTT_PASSWORD removes transfers over MQTT, not commands. A None never advises the
upload over MQTT (no page offers it, #262 was not shipped). When HTTP fails because the device's HTTP API is off, every None sentence ends with the
fix ("Turn the device's HTTP API on (SetOption128 1) or add the device by its own IP or hostname."). A command that starts a job (`Upgrade`
or `WebGetConfig` with an argument) is never sent over MQTT (`transport.ActJob`), so with HTTP failing it has no path. There is no per-device "force transport" setting.

---

## 6. Identity detection (package `fwselect`, `Detect`)

Inputs: `Status 2` (`StatusFWR`), `Status 4` (`StatusMEM`), and on minimal firmware the
`Upgrade` probe plus `RootPage`.

### 6.1 Chip family from `StatusFWR.Hardware`

| Hardware string (prefix match, case-sensitive)                            | Family                     | Source                           |
| ------------------------------------------------------------------------- | -------------------------- | -------------------------------- |
| `ESP8266EX`, `ESP8266`                                                    | `esp8266`                  | OBS                              |
| `ESP8285N08`, `ESP8285H16`, `ESP8285`                                     | `esp8266`                  | OBS / SRC                        |
| `ESP32-D0WD`, `ESP32-D0WDQ6`, `ESP32-D0WD-V3`, `ESP32-D2WD`, `ESP32-PICO` | `esp32`                    | OBS (`ESP32-D0WDQ6 v1.0`) / SRC  |
| `ESP32-U4WDH` (`-D`: dual core)                                           | `esp32`                    | SRC (`ESP32-U4WDH-D`)            |
| `ESP32-S0WD`, `ESP32-SOLO`, `ESP32-U4WDH-S` (single core)                 | `esp32`, `SingleCore=true` | SRC / OBS (`ESP32-U4WDH-S v3.0`) |
| `ESP32-S2`                                                                | `esp32s2`                  | SRC                              |
| `ESP32-S3`                                                                | `esp32s3`                  | SRC                              |
| `ESP32-C2`                                                                | `esp32c2`                  | SRC                              |
| `ESP32-C3`                                                                | `esp32c3`                  | SRC                              |
| `ESP8685`, `ESP8686` (a C3 named by its package, #470)                    | `esp32c3`                  | OBS (`ESP8685 v0.4`) / SRC       |
| `ESP32-C5`                                                                | `esp32c5`                  | SRC                              |
| `ESP32-C61` (check before `ESP32-C6`)                                     | `esp32c61`                 | SRC                              |
| `ESP32-C6`                                                                | `esp32c6`                  | SRC                              |
| `ESP32-H2`                                                                | `esp32h2`                  | SRC / DEC                        |
| `ESP32-P4`                                                                | `esp32p4`                  | SRC                              |
| anything else (e.g. `ESP32-H21`, `ESP32-S31`, empty)                      | `unknown` -> never updated | DEC                              |

Longest prefix wins. The table lives in one place (`fwselect`) with a test row per entry
and one per captured fixture. Unknown is final: no fallbacks, no substring guesses.
`ESP32-S31` and other new chips are added only together with a fixture. `ESP32-H2` is the
exception (DEC): it has no Wi-Fi and upstream builds no image for it, but people run own
builds on it over Ethernet, and those must be updatable. Its build name is `h2`
(`tasmota32h2.bin`, §7.1).

### 6.2 Image, variant and language from `StatusFWR.Version`

`Version` = `<version>(<sha-or-release>-<image>)<cores>` (SRC: `support_command.ino` Status 2 prints
version, image name and `GetCodeCores()`), e.g. `16.0.0(release-tasmota)`, `16.0.0(release-tasmota32)`,
`16.0.0(release-minimal)`, dev builds `16.0.0.3(85cff52-tasmota)`, solo1 `16.0.0(release-solo1)single-core`:

- `<version>` is `<n>.<n>.<n>`, plus `.<n>` for development and patch builds (`tasmota.ino`). Nothing else
  (no `-dev`, no two- or five-field versions).
- `<cores>` is empty or `single-core` (only builds with `CORE32SOLO1`, `support_esp32.ino`; Tasmota never
  prints `dual-core`). `single-core` on hardware that is known and not the classic ESP32 is `ErrFamilyMismatch`.
- A `Version` that does not follow this grammar does not abort `Detect` (#45): the identity keeps family,
  hardware, language and flash, `Version` stays empty and the variant `unknown`. It is never `Known()`, so
  Guard refuses every OTA (rule 1), the same contract as unknown hardware. Nothing is guessed from the string.
  The minimal-firmware path (Upgrade probe and main page) stays strict.

`testdata/fixtures/firmware-versions.json` lists every shape with its source.
`Identity.Image` keeps the raw tag. A self-compiled build reports the bare environment name
(`16.0.0(tasmota32)`, no prefix): the variant stays `unknown`, so OTA never replaces a custom build
without an explicit choice; the UI shows the reported name as "tasmota32 (self-compiled)". The one exception is the bare
tag `safeboot` (#584): it names no family and no variant to keep, so it is the same identity as `release-safeboot`
(never `Known()` without stored hardware, `ErrVariantChoiceNeeded` in `Select`, `ErrFamilyMismatch` on an ESP8266).
A bare `minimal` stays unknown (it would prove the ESP8266 family from a custom tag). One helper (`fwselect.imageOfTag`)
owns this rule for the main page and INFO1 and for Status 2 alike. The variant is normalised:

The image tag is the build's `CODE_IMAGE_STR` (SRC, arendst/Tasmota `63c3861`; every row with its source line is in
`testdata/fixtures/firmware-versions.json`), not the upstream file name (#85). The chip always comes from
`Hardware` (§6.1):

| image tag (after the `<sha-or-release>-` prefix)                               | runs on (Hardware)          | Variant -> file (§7.1)                                                  |
| ------------------------------------------------------------------------------ | --------------------------- | ----------------------------------------------------------------------- |
| `tasmota`                                                                      | esp8266                     | `tasmota` -> `tasmota.bin.gz`                                           |
| `minimal`                                                                      | esp8266                     | `minimal`                                                               |
| `lite`, `sensors`, `knx`, `zbbridge`, `zigbee`                                 | esp8266                     | same -> `tasmota-<v>.bin.gz`                                            |
| `tasmota-4M`                                                                   | esp8266                     | `4M` -> `tasmota-4M.bin.gz`                                             |
| `display`, `ir` (one `CODE_IMAGE_STR` for both families)                       | esp8266 or esp32 dual core  | same -> `tasmota-<v>.bin.gz` / `tasmota32-<v>.bin`                      |
| `webcam`, `bluetooth`, `nspanel`, `zbbrdgpro`                                  | esp32 dual core             | same -> `tasmota32-<v>.bin`                                             |
| `lvgl-haspmota` (`lvgl` up to v11)                                             | esp32 dual core             | `lvgl` -> `tasmota32-lvgl.bin`                                          |
| `tasmota32` (every `FIRMWARE_TASMOTA32` build but solo1, one per chip)         | esp32 dual core             | `tasmota` -> `tasmota32.bin`                                            |
|                                                                                | s3, c2, c3, c5, c6, c61, h2 | `tasmota` -> `tasmota32<chip>.bin`                                      |
|                                                                                | s2, p4                      | `unknown`: two builds each (s2/s2cdc, p4/p4r3), the tag cannot tell     |
|                                                                                | esp32 single core           | `unknown`: no official build reports this; not provable                 |
|                                                                                | esp8266                     | `ErrFamilyMismatch`                                                     |
| `solo1` (with suffix `single-core`)                                            | esp32 single core           | `tasmota` + `SingleCore` -> `tasmota32solo1.bin`                        |
| `safeboot`                                                                     | any esp32 chip              | `safeboot` (device sits in safeboot; treated like minimal)              |
| bare `safeboot` (self-compiled, #584)                                         | any esp32 chip              | `safeboot`, like the prefixed tag; `ErrFamilyMismatch` on an esp8266    |
| anything else, file names (`tasmota32c3`, `tasmota32-webcam`), other bare tags (#52) |                             | `unknown` -> refuse updates until the user chooses a variant explicitly |

History and limits (SRC):

- The `tasmota32` tag exists since v12.5 (`tasmota_configurations_ESP32.h`). ESP32 builds up to v12.4 report
  `release-tasmota`, which `Detect` refuses as `ErrFamilyMismatch` on ESP32 hardware (known limitation, fails
  safe).
- Until v14.0.0 the USB-CDC builds of C3, C6 and S3 were separate OTA files (`tasmota32c3cdc.bin`, ...); since
  v14.0.0 one build per chip has USB HWCDC with serial fallback (Tasmota CHANGELOG, #21212). The cdc files are
  not in §7.1 and never selected.
- S2 and P4 identify with variant `unknown`. After an explicit variant choice, Select still has to prove the
  build (§7.2): P4 by the chip revision Status 2 reports in `Hardware` (`ESP32-P4 v<major>.<minor>`): `p4`
  (board `esp32p4`, chip_variant `esp32p4_es`, built for revisions 0.1-1.99) or `p4r3` (ESP-IDF default
  3.1-3.99, `Kconfig.hw_support`); any other revision is refused. S2 is refused
  (`ErrBuildChoiceNeeded`, which is an `ErrUnknownHardware`) unless the user chose the build: nothing the
  device reports tells `s2` from `s2cdc`. The choice is `Choice.Build` (§7.2). After an update the device
  reports the shared tag, so its variant reads `unknown`: `DetectedVariant(Selection)` is the variant
  `Detect` will read back for a selection (`unknown` for S2 and P4, the selected variant otherwise), and
  the OTA verify step compares with it instead of the selected `tasmota` (family, version, language and
  reboot proof are checked as before).

With unknown hardware the tag is not checked (the identity is never `Known()`). If the
tag's chips do not include the Hardware family (e.g. `webcam` on an ESP32-C3), `Detect`
returns `ErrFamilyMismatch`. This must never happen, so if it does, stop.

Language builds (`tasmota-DE.bin.gz`, `tasmota32-DE.bin`) keep the image tag `tasmota` /
`tasmota32`. The language is read from the locale ID that `Status 4` reports as the first entry of
`Features` (`LANGUAGE_LCID`, `support_features.ino` `ResponseAppendFeatures`; `0407` -> `DE`, `0809`, the
en_GB build, -> empty), compared by value with the table in `internal/fwselect/language.go` (#686). The
main page of full firmware is not read.

**Language is known only from the locale ID.** Every upstream language build has its own `LANGUAGE_LCID`,
so the locale alone names the build, over HTTP and over MQTT alike; `Status 2` and `INFO1` carry none.
`Identity.LanguageKnown` is true only when `Status 4` was read and its locale names a build of the table. A
locale of no build (a self-compiled `MY_LANGUAGE`), a `Features` list of another shape, or no `Status 4`
leaves it false; an empty `Language` is then no proof of the English build (D10). Minimal and safeboot
firmware answer no `Status 4`: their language is the `<html lang>` of their main page when the fallback of
the minimal detection rule reads it (`pt` names no build there), and unknown otherwise. `mqttidentity` and
the MQTT identify keep both fields of the stored identity, they never set them.

### 6.3 Flash from `Status 4`

`FlashSize`, `ProgramFlashSize`, `Free` (KB). The fleet's 1 MB devices report
`FlashSize 1024` (fixtures `livingroom-readinglamp`, `bedroom-cabinet`). Values are stored in
`Identity` and refreshed on every preflight. When firmware is minimal, `Status 4` is not
available and the last stored values are kept.

### 6.4 MQTT capability (`Device.MQTT`)

From `Status 6` (`StatusMQT`): missing, `MqttHost` empty, or SetOption3 off -> `none`.
Otherwise `other`, until a message on our broker matches the device's topic -> `ours`.
Health uses MQTT only for `ours` devices. A device is never marked degraded because MQTT
is missing (TasmoAdmin `mqtt_expected` lesson).

The host, port and capability are stored by a probe and kept current afterwards (#598): `Status 6`
is read again (`Prober.ReadBroker`, stored by `store.UpdateMQTTBroker`, which writes only these
three columns and keeps `ours` when the read only says `other`) after every restart the health
tracker records, after an `MqttHost`/`MqttPort` command sent through TasmoFleet (console, bulk
command, settings, Make compliant) and about every six hours for a device whose HTTP poll works.
A requested read waits for that too, and none goes to minimal or safeboot firmware. One device at a
time, under the device lock (skipped while a job or a typed firmware update holds the device), never
`Status 0`. A read that fails half way, or whose `Status 6` says nothing about MQTT (Unknown), stores
nothing.

### 6.5 Commands over MQTT (package `mqttcmd`, #256)

`mqttcmd.Channel` is the one place that publishes a command to `cmnd/<topic>/...` and reads its
answer. Rules (each has a test; see also `docs/research/tasmota-mqtt-transport.md`):

- Never `Status 0` in any spelling `cmdpolicy` reads as 0, alone or inside a Backlog (`ErrRefused`,
  nothing published). Group topics, topics with `/+#` and `FileDownload`/`FileUpload`/`Upload`
  are refused too, and so is every command that runs other commands, alone or in a Backlog: `Json`
  (`{"Status":0}` is a Status 0), `Publish`, `Publish2`, `Rule` with a rule text (a bare `Rule1` or
  `Rule1 1` that only reads or switches is fine), `RuleTimer`, `WebSend`, `WebQuery`, `Br`, `Script`, `UfsRun`.
  They stay available over HTTP, where an admin confirms them.
- One command in flight per device topic; at least 100 ms between two publishes on the bus.
- The listener is registered before the publish, and `Bus.Connected()` is true only once the
  subscriptions of the current connection are in place (#272).
- The answer is the first live (never retained) `stat/<t>/STATUSn` for `Status n`, else a `RESULT`
  whose top-level keys name the command and its index (`POWER1` answers `Power1`, not `Power2`),
  or `{"Command":"Unknown"|"Error"}` for that command. Other messages on the topic (button
  presses, other pollers) are ignored. `DoLoose` (console) takes the first `RESULT` for
  `Template` and `Gpios` only.
- A device with `SetOption4 1` answers on `stat/<t>/<COMMAND>` instead of `RESULT` (#632); `Upgrade` answers on
  `stat/<t>/UPGRADE`, where the device later reports its download, so only the immediate reply (`Version X from <url>`,
  `1 or >X to upgrade`, `Safeboot`) answers it and never `Failed ...` or `Successful. Restarting`, and a failed `Power3`
  answers with its Command Error on `stat/<t>/POWER` (#648).
- A Backlog has no merged reply over MQTT: one `RESULT` per queued command is collected and
  merged (later keys win).
- LWT Offline -> `tasmota.ErrUnreachable`, nothing published. No answer in 8 s -> `tasmota.ErrTimeout`
  (unknown outcome, never resent by the channel). Replies to credential-setting commands are
  scrubbed (`cmdpolicy.ScrubReply`).
- `Close()` while a command waits for its answer returns an unknown outcome (`ErrTimeout`), not
  "not sent": the command was published. A closed channel never publishes.
- `Info(topic)` returns the INFO1/INFO3 seen since the device's last LWT Offline (every software
  restart publishes one).

---

## 7. Firmware selection and the safety guard (package `fwselect`)

### 7.1 Artifact naming (upstream, verified on release v16.0.0)

| Family              | Directory on ota.tasmota.com | File                                                                                                                                                     |
| ------------------- | ---------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| esp8266             | `/tasmota/release/`          | `tasmota.bin.gz`, `tasmota-<variant>.bin.gz` (`minimal`, `lite`, `sensors`, `ir`, `knx`, `display`, `zbbridge`, `zigbee`, `4M`), `tasmota-<LANG>.bin.gz` |
| esp32               | `/tasmota32/release/`        | `tasmota32.bin`, `tasmota32-<variant>.bin`, `tasmota32-<LANG>.bin`, `tasmota32solo1.bin`                                                                 |
| esp32 chips         | `/tasmota32/release/`        | `tasmota32<chip>.bin` (`s2`, `s2cdc`, `s3`, `c2`, `c3`, `c5`, `c6`, `c61`, `p4`, `p4r3`)                                                                 |
| never an OTA target |                              | `*.factory.bin` (serial flashing), `tasmota32*-safeboot.bin`, `*.map.gz`                                                                                 |

ESP8266 uses `.bin.gz` (gzip OTA since Tasmota 8.2). ESP32 uses the plain `.bin` app image.
GitHub release assets (`arendst/Tasmota`, tag `v16.0.0`) have the same file names.
Upstream builds nothing for esp32h2; `tasmota32h2.bin` is the name an own H2 build is stored and
offered under, following the `tasmota32<chip>` scheme.

### 7.2 Select

`Select(identity, choice)` returns the file name. It keeps the detected variant, language
and chip build unless the user explicitly chose otherwise (`Choice.AllowVariantChange`,
set only from a UI confirmation or a CLI flag). For esp8266 it always also names
`tasmota-minimal.bin.gz` of the same release. Language and variant cannot be combined
upstream (there is no `tasmota-sensors-DE`). Asking for that returns `ErrNoSuchVariant`.
The chip build of the newer chips comes from `Hardware` alone: the one build of S3, C2, C3, C5, C6 and C61; for
P4 the build whose revision range contains the chip revision (§6.2); every other case returns
`ErrUnknownHardware`. ESP32-S2 returns `ErrBuildChoiceNeeded` (an `ErrUnknownHardware`) unless
`Choice.Build` is `s2` or `s2cdc` (`BuildOptions(identity)`); that explicit build implies the plain `tasmota`
variant and the change from the detected `unknown` of an official `tasmota32` tag, and only it does (a
self-compiled or custom S2 image stays `unknown` and needs an explicit variant choice as well, §6.2). A build on any other chip, or one that
contradicts a provable revision (`p4` on a 3.1 P4), returns `ErrNoSuchVariant`; a P4 outside the known
revisions stays refused with or without a build. The build is chosen per update in the update preview and
kept on the job (`Job.TargetBuild`, `ota_jobs.target_build`, migration 0033); Guard rule 7 holds the artifact
to it. The choice is remembered per device (#186): `devices.build_memory` (migration 0039, nullable, `s2`|`s2cdc`|NULL,
`Device.BuildMemory`). It is written only when an update is started with an explicit build (Start update) and cleared by
"Forget" in the preview or by Replace device; nothing a device reports fills it. The next preview preselects it
(`Request.Build`, "Remembered from your last update", radios stay changeable) only when the chip has that build, and
it travels as an ordinary explicit build, so Guard rule 7 checks it exactly as before. This also covers the minimal/safeboot auto-default ("tasmota" chosen for the device).

**Language unknown.** When `Identity.LanguageKnown` is false and `Choice.Language` is empty, Select returns
`ErrLanguageUnknown` unless English is proven without a page (below; checked right after the unknown-hardware check, before any variant rule, because the
language is what the person must supply). The language is proven when no language build can be running: an official tagged image (`Image` has a
`<release>-` prefix), no stored language, not minimal/safeboot, and no language build for the hardware and variant
(`hasLanguageBuild`: only `tasmota` or `unknown` on an ESP8266 or a dual-core ESP32 has one). So C3/S3 and the other
chips, solo1 and every non-`tasmota` variant need no page. `Choice.Language = "EN"` is a valid explicit answer, as is any
upstream language suffix. Guard rule 7 runs Select, so an unread language also stops an OtaUrl or Upgrade. Devices stored before 0.20 whose language was read from a page are
marked known by migration 0037 (identity_at set, and the identity of every stored job). A
synthetic identity that stands for no device (the custom-upload file name) is built with `LanguageKnown`
true. This mirrors the ESP32-S2 build choice (`ErrBuildChoiceNeeded`): never guess. The update preview
offers the choice (WP-D2).

### 7.3 Decide (update policy)

Per batch: `only-newer` (default), `reinstall`, `allow-downgrade`. Versions compare
numerically per dotted field. A device on minimal/safeboot always decides "upgrade". One
function, table-tested (TasmoAdmin lesson A5).

### 7.4 InspectImage (verified against `testdata/fixtures/firmware-headers/`)

- Byte 0 must be `0xE9`, otherwise `ErrBadImage`. `.bin.gz` is gunzipped first; byte 0-1
  of the file must be the gzip magic `1f 8b`.
- ESP32 app image: byte 8 = `0xEE` and `chip_id` = uint16 LE at offset 12. Mapping:
  `0x0000` esp32, `0x0002` s2, `0x0005` c3, `0x0009` s3, `0x000C` c2, `0x000D` c6, `0x0010`
  h2, `0x0012` p4, `0x0014` c61, `0x0017` c5. Unknown chip_id -> `ErrBadImage`.
- Otherwise ESP8266 (no extended header; byte 8 is a segment load address byte).
- solo1 and dual-core ESP32 share chip_id 0. The file name and `SingleCore` decide.

The firmware cache runs InspectImage on every download and rejects files whose header
family disagrees with their name.

### 7.5 Guard (hard, runs immediately before `OtaUrl` and again before `Upgrade`)

Refuses, with a typed error, unless **all** hold:

1. `identity.Known()` and family != unknown -> else `ErrUnknownHardware`.
2. `artifact.Family == identity.Family` (header-proven) -> else `ErrFamilyMismatch`.
3. The file-name family (`tasmota32c3...` -> esp32c3, `tasmota-*`/`tasmota.bin.gz` -> esp8266)
   equals both, and `path.Base(otaURL) == artifact.Filename`, and the URL is the
   firmware server's URL for that artifact -> else `ErrFilenameMismatch`.
4. Not `artifact.Safeboot`, not `*.factory.bin` -> else `ErrSafeboot`.
5. esp8266 artifacts end in `.bin.gz`, esp32 artifacts end in `.bin` -> else `ErrFilenameMismatch`.
6. Single-core ESP32 gets only `tasmota32solo1*`, dual-core never gets solo1 -> `ErrFamilyMismatch`.
7. Variant/language equal the detected ones, or `AllowVariantChange` is set -> else
   `ErrVariantChange`. Exception: the minimal stage artifact for esp8266.
8. URL length ≤ 100 characters (Tasmota settings text pool) -> else `ErrFilenameMismatch`.
9. Settings-migration stop (#892): a device running a version below `MigrationFloor` (10.0.0.3) gets no
   artifact whose release is above `StepRelease` (16.0.0) -> else `ErrSettingsMigration`. Tasmota removed every
   `SettingsDelta` step below 10.0.0.3 after 16.0.0, for every chip family, so such a device must take 16.0.0 first.
   Before that, the older stop of Tasmota's migration path: a device below `PathFloor` (8.1.0) gets no artifact
   whose release is above `PathStep` (8.5.1) -> else `ErrMigrationPath`. Since 9.2.0.6 no release migrates such a
   settings block (CHANGELOG `[9.2.0.6]`; RELEASENOTES of v16.0.0: "removes support for direct migration from
   versions before v8.1.0"); `SettingsDelta` of 16.0.0 resets it with `SettingsDefault()` and keeps only Wi-Fi
   and the MQTT connection (`settings.ino`, "Although no direct upgrade is supported try to make a viable
   environment"). 16.0.0 is therefore no step for it; the index has no 8.5.1, so only an uploaded official
   8.5.1 image serves it. The check uses the artifact's release (every stage, every caller) and fails closed on
   a version it cannot read. `Decide` runs the same function, so the preview refuses before it fetches the newer
   file. A device stopped by `ErrSettingsMigration` counts under "Needs a choice", and the preview offers one
   action, a preview of 16.0.0 for exactly those devices with the same policy, variant, delivery, backup and
   time. That is an ordinary one-release update, so every batch keeps one release. A device stopped by
   `ErrMigrationPath` is refused and offered no step. The firmware index keeps 16.0.0 after it left the newest
   releases: the archive directories `release-16.0.0/` of ota.tasmota.com and the GitHub tag `v16.0.0`.

The engine also checks that the `Upgrade 1` reply `Version X from <url>` echoes exactly
the URL it set. If not (another tool changed OtaUrl in between), the job fails. The device has accepted
the upgrade by then and flashes from the other URL, so the engine first asks for the device to be held
against new updates (`Options.HoldFlashing`, the same hold a typed `Upgrade 1` starts): failing releases the
device, and a second update on a device that is still flashing can brick it.

Guard and Plan run under mutation testing with a required efficacy ≥ 90 % (§21).

### 7.6 Plan (per-family update policy)

| Family  | Plan                                                                                                                                                                                                    |
| ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| esp32\* | **direct**. If the device uses a single OTA partition with safeboot, Tasmota itself goes through safeboot and continues with the stored OtaUrl (SRC). The engine tolerates seeing `safeboot` in between |
| esp8266 | **staged** if `target .bin.gz size > Free*1024 - 16 KiB` (margin), or if Free is unknown and `FlashSizeKB <= 1024`; otherwise **direct**                                                                |

Never decided by model name. Fleet check (OBS, fixtures): every ESP8266 in the fleet, the
ESP8285N08 lamps and the ESP8266EX plugs alike, reports `FlashSize 1024`,
`Free 352` (360 448 bytes). `tasmota.bin.gz` 16.0.0 is 469 152 bytes and
`tasmota-sensors.bin.gz` is 512 993, so they are **staged**. `tasmota-minimal.bin.gz`
(263 670) fits. The compressed image is what lands in flash (the bootloader unpacks it),
so the gz size is the one to compare. For ESP32, `Free` (697 KB on the QuinLED) is not
the OTA partition size and is ignored; ESP32 is always direct.

### 7.7 MQTT upload fit (`MQTTUploadFits`)

`MQTTUploadFits(identity, layoutHint, imageSize) (ok, needKB, roomKB, reason)` decides before anything is sent
whether Tasmota will accept the image as `FileUpload` Type 1. It is pure and never "tries and sees". The rule,
for both ESP32 flash layouts and without knowing which one the device has:

```
round_up_4096(imageSize) <= FreeKB * 1024 - 192 KiB          FreeKB = Status 4 "Free"
```

Refused outright, each with its own reason: ESP8266 (its minimal firmware has no `FileUpload`), identity not
known, variant `minimal` or `safeboot` (neither has `FileUpload`), a known safeboot layout, `FreeKB` <= 0, image
size <= 0. `UpLayoutHint` (`unknown`, `direct`, `safeboot`; from a read of `/up`, #30): `safeboot` (one app
partition) is refused outright, because `MqttFileUploadValidate` calls `Update.begin` without the single-OTA
handling of the web upload and the next update partition can be the running one. The other values never change
the numeric decision. An unknown layout keeps the numeric rule until WP-D2/G1 have checked the Arduino core's
Updater.

Derivation (SRC: Tasmota v16.0.0, `xdrv_02_1_mqtt_file.ino` `MqttFileUploadValidate`; `support_esp32.ino`):

- The device refuses (error 2, "Not enough space") when `round_up_4096(file_size) > head_room`, with
  `head_room = (FlashWriteMaxSector() - FlashWriteStartSector()) * 4096`.
- `FlashWriteStartSector()` = `EspFlashBaseAddress() + 2 * 64 KiB`, rounded down to 64 KiB.
  `FlashWriteMaxSector()` = `EspFlashBaseEndAddress()` rounded down to 64 KiB.
- Two app partitions (direct): `EspFlashBaseAddress()` is `esp_ota_get_next_update_partition()->address`, the
  start of the other partition, and `EspFlashBaseEndAddress()` is its end. `ESP_getFreeSketchSpace()` (what
  Status 4 prints as `Free`) is `ESP.getFreeSketchSpace()`, the size of that same partition. So
  `head_room > Free - 128 KiB - 64 KiB` (rounding the end down loses less than 64 KiB); the size of the running sketch is irrelevant.
- One app partition next to safeboot: `EspFlashBaseAddress()` is `running->address + ESP_getSketchSize()`, the
  end is the end of the partition, and `Free` is `partition size - sketch size`
  (`ESP_getFreeSketchSpace()`, `EspSingleOtaPartition()` branch). So again `head_room > Free - 128 KiB - 64 KiB`.
- Status 4 prints `Free` in whole KB (rounded down), which only makes the figure smaller and the bound more
  conservative. 128 + 64 = 192 KiB. The bound holds without knowing the layout.

The rule does not use `ProgramSize`: on a dual layout it plays no part. The simulator's esp32-dual profile reports
`Free` 2880 KB (the other partition) next to the safeboot capture's `ProgramSize` 2182 KB. Not verified on a real
dual-OTA device (#263).

---

## 8. Firmware sources, cache and server (package `firmware`)

### 8.1 Sources

| Name              | Kind              | Index                                                                                                                                     | Download hosts (allowlist)                                                                              |
| ----------------- | ----------------- | ----------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| `github`          | `github-releases` | `GET https://api.github.com/repos/arendst/Tasmota/releases?per_page=10` (JSON, machine-readable); plus `GET .../releases/tags/v16.0.0` when that list lacks the step release 16.0.0 (#892) | `api.github.com`, `github.com`, `objects.githubusercontent.com`, `release-assets.githubusercontent.com` |
| `ota.tasmota.com` | `ota-tasmota`     | HTML listing of `/tasmota/release/` and `/tasmota32/release/` (only `href` file names are parsed; version from the release notes heading); plus `/tasmota/release-16.0.0/` and `/tasmota32/release-16.0.0/` when the release directory no longer lists 16.0.0 (#892) | `ota.tasmota.com` (http allowed)                                                                        |

`FIRMWARE_SOURCES` (System settings, applied at the next restart) orders them. A file only one source offers is refused unless `FIRMWARE_ALLOW_SINGLE_SOURCE` is on. GitHub is the primary index because it is JSON. If both
sources are enabled, every download is fetched from both and the SHA-256 must match
(cheap cross-check against a tampered mirror). A mismatch rejects the file and raises an
error in the UI. The index is refreshed at start (async, backoff 30 s..30 min until the first success) and then every `FIRMWARE_REFRESH_INTERVAL` (default 6 h); a failed refresh keeps the previous index (gauge `tasmofleet_firmware_index_age_seconds`). It is never fetched on page render
(TasmoAdmin lesson B6), only by an explicit refresh or a background refresh (default 6 h).

### 8.2 Cache

`/data/firmware/<release>/<filename>`, immutable, indexed in `firmware_artifacts` with
size, SHA-256, header-proven family, variant, language and safeboot flag. Downloads go to
a temp file, then fsync and rename. Size limit 8 MiB. A redirect hop outside the allowlist
aborts. The cache prunes itself: `FIRMWARE_KEEP_RELEASES` (System settings, live, default 3, `0` = never) keeps the
files of the newest N cached releases. `Cache.Prune(keep)` runs after every successful index refresh and in the daily
retention run; it goes through `Delete` per file (per-file lock, "in use" re-checked), so files an active or
queued job holds stay, a release with a held file stays whole, uploads are never pruned, and a row whose name would
leave the cache directory is skipped. It fails closed when the in-use lookup fails. A run that deletes something
(or fails) is audited as `firmware.prune` by `system` with file count, bytes freed and the count kept in use.
Files also leave the cache by the admin's Delete and "delete unused" (§8.2a). Uploads never wipe the cache (TasmoAdmin lesson B6).

### 8.2a Firmware page: cache management, prefetch, changelog (M2)

- **Release notes.** The GitHub index already carries each release's `body`; the cache keeps it
  (cut to 32 KiB per release, at most the 10 indexed releases) next to the index and the
  release page URL. The `ota.tasmota.com` source has none. Notes are rendered by `firmware.ParseNotes`,
  a minimal Markdown subset (headings, bullet/ordered lists, fenced code, quotes, `**bold**`,
  `` `code` ``, `[text](url)`, bare URLs) that produces plain data, never HTML. templ escapes every
  string; there is no `template.HTML` on the path. Raw HTML in a body is shown as text. A link survives only if
  `firmware.NoteLink` accepts it (absolute `http`/`https`, public DNS host, standard port, no user info, no whitespace or control
  characters) and its host must be on an allow-list (project and GitHub hosts, configurable); it is rendered with `rel="noopener noreferrer nofollow"` and `target="_blank"`. Images are never
  loaded (shown as links). **No new dependency:** goldmark plus bluemonday would be a large parsing and
  sanitising surface for four constructs, and a sanitiser allow-list is the weaker design than "the parser cannot
  emit markup".
- **Delete.** `firmware.Manager.Delete` / `DeleteUnused`: admin only, a confirmation page, audited
  (`firmware.delete`, also the refusals). Refused while an active or queued job holds the artifact as target
  or tasmota-minimal ("in use by job N"); refused if the lookup fails. The same per-file lock as `Ensure`
  serialises a delete against a download of that file. Residual: the window is the whole planning step (it can include a download); the job then fails on the artifact id mismatch, never flashes the wrong file. For direct ESP8266 plans the job row records the minimal image too.
- **Fleet needs.** `firmware.FleetNeeds(release, devices)`: per device `fwselect.Select` (keeping variant
  and language; a device on minimal/safeboot gets the standard build) plus `tasmota-minimal.bin.gz` for every
  ESP8266, excluded devices and devices with unknown hardware need nothing. This is the single computation behind the
  per-release coverage ("cached N of M needed files"), the "devices that would use this file" column, and
  the prefetch file list. It makes no safety decision; the OTA engine still runs the Guard.
- **Prefetch.** `POST /firmware/prefetch` (admin) computes the file list on the server from the inventory (the form
  only names a release that is in the index), starts `firmware.Prefetcher` (one run at a time, sequential
  `Cache.Ensure`, so size limit, cross-check and image-header check are unchanged, 30 minute timeout, audited
  `firmware.prefetch`) and redirects to a progress page fed by an SSE stream like the discovery scan.
- **Update indicator.** `firmware.UpdateAvailable(latest, identity)` is the one definition of "update
  available" (known version older than the newest stable release) for the firmware page and the device list;
  `firmware.Latest` is the "recommended" release.

### 8.3 Device-facing firmware server

Its own listener (`FIRMWARE_LISTEN`, default `:80`). It serves only `GET`/`HEAD`
`/fw/<release>/<filename>` for indexed artifacts, with `Content-Length` (ESP8266 checks
free space from it) and `Content-Type: application/octet-stream`. There is no directory
listing, no other routes, and no authentication (devices cannot authenticate). Every
request is logged with the device IP and published as an event. The engine uses "device X
started downloading" as a progress hint. All files of one release share a directory, so
Tasmota's own fallback (it rewrites the OtaUrl file name to `tasmota-minimal.bin.gz` in the
same directory, SRC) also resolves. It also serves, for uploaded firmware, `/c/<key>/fw/<release>/<filename>` (§8.6). Requests are rate limited per client IP (120 per minute).

`FIRMWARE_BASE_URL` is the URL devices use (`http://<container-ip>`). It must be plain
`http://` (validated). There is no auto-detection: in ipvlan/bridge setups the right
address is only known to the operator.

Without `FIRMWARE_BASE_URL` there is no firmware cache and no firmware server, but the transaction engine
still runs (#881): bulk backup, Send command, bulk Restart, Make compliant and Apply template are
transactions that need no firmware file. The engine then gets a firmware that refuses every file (`firmwareOff`
in `app/ota.go`), so an update is refused at planning, before `fwselect.Guard`, and no `OtaUrl` or `Upgrade`
can be sent. The UI follows: the transaction pages exist (`web.Deps.OTA`), the update wizard, its posts, the
Firmware pages and the "Update firmware" buttons need `web.Deps.Firmware` too, and `/updates/new` explains
what to configure. The URL is read at start-up, so a change applies after a restart; update jobs left over
from a run with the URL are resumed by the engine and end failed with the reason ("firmware updates are
switched off") when they need a file.

### 8.4 Variant list for the UI

The update dialog offers, per device, only files of the device's family that exist in
the chosen release (from the index). The default is the detected variant.

### 8.5 Channels

`stable` (releases) only. A `development` channel (ota.tasmota.com root) was planned and is not built.

### 8.6 Custom firmware upload (M2)

Admin uploads `.bin`/`.bin.gz` (`POST /firmware/custom`, multipart, CSRF token in the
header, streamed to a temp file) with a label, a dotted numeric version (the release
directory) and a build name (default `tasmota`). Container (gzip magic) and chip family come
from the bytes via InspectImage; the file name and MIME type are display data. Limits on the
uncompressed image: 4 MiB ESP8266, 8 MiB ESP32. Refused: unknown or mismatching chip,
safeboot/factory/bootloader names, a partition table at 0x8000 (full-flash image), corrupt
or tiny files, the same bytes twice. The version may equal an upstream release (self-built
images report the version they were built from): an upload is identified by `Artifact.Custom`,
the first 10 hex digits of its SHA-256, and fetched at `/c/<key>/fw/<release>/<file>`
(`Request.Custom`/`Job.TargetCustom` carry the key; the engine resolves such jobs with
`EnsureCustom`, never `Ensure`). The Guard is unchanged. The image is stored
as `custom/<key>/<version>/<name from fwselect.Select>` (ESP8266 gzip, ESP32 raw, whatever was uploaded)
with `Source=custom` and provenance in `firmware_custom` (the release column of an upload is stored as `<key>~<release>`, so the old UNIQUE(release, filename) holds) (label, uploader, time, original
name). An ESP8266 custom release gets `tasmota-minimal.bin.gz` as a verified copy of the
newest upstream minimal (`Source=custom-minimal`) on first use, so the two-step update
works. Uploads are exempt from Prune and "delete unused"; Delete refuses while a job needs
them. The update page offers them as `custom: <label>`, never as default or recommended.
The same Guard applies; audit: `firmware.custom.upload`, `firmware.custom.use`,
`firmware.delete`. Security notes: docs/security/custom-firmware.md.

---

## 9. OTA state machine (package `ota`)

### 6.6 The transport router (package `transport`, #256)

`transport.Router` implements `tasmota.Client` and is `app.Services.Client`; `Services.HTTP` is the raw
HTTP client with the router's failure memory attached, used by what measures or verifies HTTP (health
poll, prober, MAC verifier, discovery scan). The router passes `Uploader` and `BootSwitcher` straight to
HTTP. Rules:

- A call whose Target has no device ID (scan, MAC verifier, address probe) is always HTTP.
- A command that `cmdpolicy` classifies as a read follows `Paths.Commands`, any other command `Paths.Writes`
  (5.x): both go over MQTT first for a device usable there, writes under the strict rule (#605).
- **Writes while HTTP is not failing** (#605): over MQTT when `Choose` gives MQTT for writes; else HTTP first
  (with the failover below). HTTP only when an operation cannot be done over MQTT or MQTT is not usable for the
  device (the product owner's rule of 2026-11-14): what a write carries is no reason. A password, a key, a login in
  the text, a `Template` and a change of the MQTT connection (`cmdpolicy.CutOffName`) go MQTT first like any other
  write, from TasmoFleet's own code and from the console or a bulk command alike; the latter needs no consent while
  HTTP is proven to work right then (one `Status 11` the router asks over HTTP right before, through the failure
  memory but not the trace; an answer on record does not count, HTTP may have stopped since), because a device MQTT
  loses after the change is reached over HTTP for the next command. Without that answer the consent rule below
  applies as with HTTP failing. Once such a change was published after that answer (answered or not), or HTTP
  carried it to a device on TasmoFleet's broker (an answer or a timeout: a refused Backlog, a device marked already,
  a device whose MQTT commands went unanswered), the router marks the device (`Router.MQTTConnChanged`, read by the facts as `MQTTFacts.ConnChangedSince`): Tasmota restarts
  after it with a clean disconnect, so its retained last will stays Online even when it cannot rejoin the broker,
  and while the mark lasts `Choose` gives HTTP for its writes and reads while HTTP is not failing (the card says
  why). The mark lasts 10 minutes after the last such change; an answer over MQTT to anything but such a change
  ends it earlier, which only happens on paths that still use MQTT while it is set (a pinned job, HTTP failing). A
  change sent with the consent sets none. Which command changes the MQTT connection is read as the device reads its
  name (`cmdpolicy.ResolveName`, built from the pinned Tasmota catalogues): any case, the index as a number, every
  SetOption synonym as its SetOption (`MqttTLS` is `SetOption103`). A secret is masked where it always was (audit and history:
  `cmdpolicy.Redacted`; replies: `cmdpolicy.ScrubReply`), not routed. No write is kept HTTP first, Make
  compliant included (#606, below). A write MQTT provably
  did not publish (the channel refused it: a command that runs other commands such as `Json`, `Rule` with a rule
  text, `If`, `WebSend`, `DevGroupSend`, `LoRaCommand`, a `Template` with a `CMND` key, or `Backlog2`/`Backlog3`,
  Status 0; the bus was down; the
  device offline by its LWT; the snapshot changed) is sent once over HTTP, never back to MQTT within the call. A
  write MQTT published and the device did not answer (`tasmota.ErrTimeout`) is an unknown outcome: returned as
  such, never sent again; while the caller still waits and HTTP is not failing it marks the device like an
  unanswered read, so its next commands go over HTTP for 10 minutes. docs/security/mqtt-transport.md lists what still uses HTTP and why.
- The power toggle (`devicecmd.Power`) calls the command channel itself, so it confirms the relay's state; it
  publishes only where `Choose` gives MQTT for writes (`devicecmd.Options.WritesOverMQTT`, injected by the app), else
  it switches through the router. A switch the channel did not publish goes over HTTP once, pinned
  (`WithPath(HTTP)`), so the router cannot publish it after all. A switch through the router is traced: its audit
  `via` names the transport the router used. A device with an unread GroupTopic is switched over HTTP (before
  #605 it was switched over MQTT whenever it was on our broker).
- **Failover inside one call** from HTTP to MQTT happens only on `tasmota.NotSent(err)`: a refused
  connection (the dial failed), an address outside DEVICE_NETWORKS, a 401 before execution, or a device
  that refuses the HTTP API (SetOption128; for a _write_ only when the failure memory already held that
  kind before the call, because the silence of a refusing handler can also follow a command that ran). A timeout, an unparsable answer or a connection dropped after
  the request was written prove nothing and are never followed by an MQTT send. Never MQTT to HTTP after a
  publish. Uploads never fail over.
- Every HTTP outcome goes into `Memory` (the failure kind and its start; a failure lasts 10 minutes and is
  cleared by the next answer, a health poll included), which is how `Facts.HTTP` becomes "failing".
- `transport.WithPath(ctx, HTTP|MQTT)` pins a job's transport: no failover, no rerouting. A command that starts
  an upgrade or a restore (`Upgrade <x>`, or `WebGetConfig <x>` typed in the console on a custom build) is sent over MQTT only when the path is pinned
  to MQTT; the generic router never starts one on its own.
- The device is read once, the facts and the choice come from that snapshot, and the device is read again
  right before an MQTT send; a changed topic, group topic, FullTopic or MQTT capability sends nothing.
- Replies of secret commands (`cmdpolicy` verdict `Redact`) are scrubbed by the router on every path, MQTT and HTTP
  alike (`cmdpolicy.ScrubReply`, #809).
- `WithTrace(ctx)` records which transport carried the calls; the audit details of console commands,
  power switches, settings writes and Make compliant carry it as `"via":"http"|"mqtt"` (`"http+mqtt"` when a job used both).
- A write that can cut off a device reached over MQTT only (HTTP not usable: failing, or silent to the router's
  `Status 11`; `cmdpolicy.CutOffName`: Topic, FullTopic,
  GroupTopic, Prefix<n>, Mqtt host/port/user/password/client/fingerprint, `SetOption3 0`, `SetOption103`,
  `SetOption132`) is published over MQTT only when the call carries `transport.WithCutOffConsent`, on the routed
  path and after a failover alike; otherwise `ErrCutOff` ("not sent: this device is reached over MQTT only; changing X can cut
  TasmoFleet off from it. Use the console, which asks to confirm"), nothing published, audit `via` "none". The
  console sets the consent on the confirmation that showed the consequence; it shows it up front whenever HTTP is not
  known to work (`app.reachedOnlyOverMQTT`: no fresh answer, not `HTTPOK`), and on the router's
  `ErrCutOff` when HTTP went silent after its last answer; opening the console asks for an HTTP probe so a working
  HTTP is usually fresh. Bulk commands are refused in the preview only for devices known to be reached over MQTT
  only (`app.knownOnlyOverMQTT`: HTTP failing, MQTT usable); for the others the router decides per device. Make
  compliant never sets it: `policy.Fixer.Apply` sends such lines last in chunks of their
  own, a refusal is reported as `SkippedError` ("applied N; skipped Topic, MqttHost: reached over MQTT only (use the
  console)"), the job succeeds for the rest and verifies only what was sent, and the preview lists them as skipped
  for devices known to be reached over MQTT only.
  The settings page reports the refusal per field.
- A **console or bulk write whose arguments the audit hides** (`transport.ConfirmsRoute`: a change, not
  `cmdpolicy.ShownInFull`) names its transport in its confirmation before anything is sent (#826, the product
  owner's rule of 2026-11-14: the route stays MQTT first, the confirmation is honest about it). The console and the
  bulk preview ask `Router.TypedRoute`, which takes `Command`'s own steps with the same inputs (the cheap HTTP state,
  `writeOverMQTT`, `decide`); a job command is HTTP, and one the MQTT channel never publishes HTTP, or `None` (sent
  nowhere: no sentence, a "Not sent" bulk row, left out of the run) while the device's HTTP is known to fail. Over MQTT the
  sentence says the command is unencrypted (with a TLS broker URL: encrypted from TasmoFleet to the broker) and that
  the broker delivers it to every client subscribed to the device's commands unless an ACL prevents that; over HTTP
  that it is unencrypted. The console asks for every such command; the bulk preview has one row per transport with
  how many devices. The confirmed send carries the route (`transport.WithRoute`), which `httpCommand` and `publish`
  check: another transport, the failover after HTTP sent nothing and the move to HTTP after MQTT published nothing
  included, sends nothing (`ErrRouteChanged`, audit `via` "none"), and the console asks again. The console and the
  bulk run compare the sentences right before the send; a command job keeps MQTT as `Job.Transport` (HTTP otherwise)
  and a retry keeps the job's route.
- **Make compliant** (#606) follows the write path like any other write: `policy.Fixer.Apply` sends its Backlog
  chunks (at most 8 lines, and a Backlog no longer than `cmdpolicy.MaxCommandLen`, 255 bytes, which the MQTT
  channel takes; the restarting lines last) and then the one `Backlog SaveData <current>` through the router, so
  for a device usable on our broker they go over MQTT first. A longer chunk would be refused by the channel and go
  over HTTP although MQTT can carry it, and the router could not read a change of the MQTT connection in it. Over MQTT the channel answers a
  Backlog once every queued command has answered or the replies fell quiet (`collectBacklog`, merged like the reply
  /cm gives), so the save that follows is queued behind lines that ran; over HTTP it is queued behind them (#559).
  A chunk with a line `cmdpolicy.CutOffName` names goes over MQTT too while HTTP answers the router's `Status 11`
  right before (no lockout then, no question; the router marks the device and HTTP carries its next commands);
  without that answer the router refuses it (`ErrCutOff`: Make compliant never carries the consent) and the fixer
  sends the other lines of that chunk again and reports the rest as skipped (above). The restarting lines of a run
  that holds such a line never join the last plain chunk, so every plain line has run when the router asks: a plain
  `SetOption128 0` that closes HTTP to TasmoFleet runs before the question, not after it in the same Backlog. The
  plan judges a restart by the value the device gets (`SetOption103 ON` is sent as `SetOption103 1`), as `Apply`
  does, so such a line is never among the plain ones. The
  question cannot cover a restarting line of the same chunk that changes how HTTP finds the device after the
  restart (`Hostname`, or `SetOption55 0` that turns mDNS off, for a device TasmoFleet reaches by that name): a
  residual risk. Every line Make compliant can send that cuts MQTT off either restarts the device, so it is in the
  last chunks and no save follows it, or is a SetOption that restarts at 0 and 1 set to a value above 1, which
  Tasmota v16 answers with Command Error and does not apply. Policy and the router read a name the same way, with
  `cmdpolicy.ResolveName` (`MqttTLS` is `SetOption103` for both), and policy accepts only the name the device runs
  (a synonym or a zero-padded SetOption is refused), so `cmdpolicy.CutOffName` sees every connection change a run
  holds. A chunk MQTT published and the device did not confirm is an unknown
  outcome: never sent again; the run stops there, the lines sent before it are saved (`saveAfterFailure`) and the
  job fails saying the step may or may not have reached the device (a restarting chunk that holds a value the read-back can read is the exception, as over
  HTTP: its silence is the restart, and the read-back decides; a restarting chunk without one stays an unknown outcome). Its lines never carry a secret: policy rules
  refuse secret settings and URLs with a login (a policy is stored and shown in the clear), and `Apply` refuses
  any stored line that is not a rule's canonical command.
- `DownloadConfig` goes MQTT first under Choose's backup path (#607, 5.x); `RootPage` stays HTTP.

Background GroupTopic read (`internal/app/grouptopic.go`): shortly after start and after a device is
adopted, one bare `GroupTopic` per two seconds (through the router, which routes it as a read: MQTT first, HTTP
where MQTT cannot serve the device; it answers all four group topics, #1005) for
every device with a topic whose group topics were never read or are older than its last probe; a device that
gave no answer is asked again after 10 s, 30 s, 2 min, then every 15 min, but at once when it comes online after a failed attempt. Offline devices are not asked. When no device needs a read the reader does not list the inventory for 30 s. The identity refresh reads
`Status 1` and the bare `GroupTopic` too and stores all four. Right after the upgrade to 0.42 every device's
GroupTopic2 to 4 are unread, so MQTT writes and transfers wait until the reader has asked each device once.

### 9.1 Principles

- One job per device, grouped in batches. Jobs are persisted, and every transition is
  written to the database **before** its actions run (write-ahead).
- `Step(job, event, now, cfg) -> (job', actions)` is pure and total. The engine runs the
  actions and turns their results into events.
- Only idempotent commands are retried automatically (`Status N`, `OtaUrl <url>`, `GET /`).
  `Upgrade 1` is sent at most `MaxAttempts` (2) times per stage, and before a resend the
  engine always probes whether the first one took effect.
- Every wait has a deadline. Every failure has a reason string in `LastError` and a row in
  `ota_job_events`.
- HTTP only. MQTT `stat/<t>/UPGRADE` and firmware-server download events are hints that
  can only speed things up; nothing depends on them.

The same engine, batch and transaction pages also run the other multi-device operations as job
kinds (`model.JobKind`): `policy` (Make compliant), `command` (bulk Send command and Restart) and
`backup` (bulk Back up) and `template` (apply a GPIO template, #327: backup, re-read, one `Template` command, wait for the restart, read back; Parallel 1, the rest of the transaction is cancelled after the first failure). They reuse persistence, per-device locking (`devlock`), cancel and "Retry
failed"; none of them touches firmware, so the Guard and the states below concern `update` jobs.

### 9.2 States

`scheduled` (waits for its start time, 9.4b), `queued`, `preflight`, `backup`, `set_ota_url`, `safeboot_switch` and `safeboot_recover` (push into
safeboot only, below), `upgrade_issued`, `await_reboot`, `verify`,
`restore_ota_url`, terminal `succeeded`, `failed`, `cancelled`. `Stage` is `minimal` or
`final`; staged jobs pass through `set_ota_url -> ... -> verify` twice.

### 9.3 Transition table

| From                                   | Event                                              | Guard / condition                                                                | To                                                                                                               | Actions                                                             |
| -------------------------------------- | -------------------------------------------------- | -------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------- |
| scheduled                              | start_due                                          | `schedule.At` is Run (start <= now <= start + 60 min) and the start check passed (9.4b) | queued (detail: planned start and lateness)                                                                      |                                                                     |
| scheduled                              | not_started                                        | `schedule.At` is Missed (now > start + 60 min)                                   | cancelled, `NotStarted`, LastError "TasmoFleet was down at the planned start ..."                                | notify                                                              |
| scheduled                              | guard_refused                                      | not before the start (excluded from OTA, or the device was never free in the window) | failed (LastError = the reason)                                                                                  | notify                                                              |
| scheduled                              | cancel                                             |                                                                                  | cancelled                                                                                                        | notify                                                              |
| queued                                 | start                                              | slot free (scheduler)                                                            | preflight                                                                                                        | probe_identity                                                      |
| preflight                              | identity                                           |                                                                                  | preflight                                                                                                        | plan (record IdentityBefore, OriginalOtaURL if empty, UptimeBefore) |
| preflight                              | planned                                            | decision skip                                                                    | succeeded (detail "already at target")                                                                           | notify                                                              |
| preflight                              | planned                                            | `SkipBackup`                                                                     | set_ota_url (stage = minimal if staged and not already minimal, else final)                                      | set_ota_url                                                         |
| preflight                              | planned                                            | otherwise                                                                        | backup                                                                                                           | backup                                                              |
| preflight                              | guard_refused                                      |                                                                                  | failed                                                                                                           | notify                                                              |
| preflight                              | command_failed / timeout                           |                                                                                  | failed                                                                                                           | notify                                                              |
| backup                                 | backup_done                                        |                                                                                  | set_ota_url (stage as above)                                                                                     | set_ota_url                                                         |
| backup                                 | backup_failed                                      |                                                                                  | failed                                                                                                           | notify                                                              |
| set_ota_url                            | command_ok                                         |                                                                                  | upgrade_issued, Attempt+1, deadline now+UpgradeAccept                                                            | upgrade (guard re-run first)                                        |
| set_ota_url                            | command_failed / timeout                           |                                                                                  | failed                                                                                                           | notify                                                              |
| set_ota_url                            | guard_refused                                      | (engine re-runs Guard before OtaUrl)                                             | failed                                                                                                           | notify                                                              |
| upgrade_issued                         | command_ok                                         | reply echoes the URL                                                             | upgrade_issued                                                                                                   | probe after ProbeInterval                                           |
| upgrade_issued                         | command_ok                                         | URL differs                                                                      | failed ("OtaUrl changed by someone else")                                                                        | notify, and hold the device against new updates                     |
| upgrade_issued                         | command_failed                                     |                                                                                  | upgrade_issued (unknown whether it started)                                                                      | probe after ProbeInterval                                           |
| upgrade_issued                         | guard_refused                                      | Guard re-run before `Upgrade` refused; Upgrade not sent                          | failed                                                                                                           | notify                                                              |
| upgrade_issued                         | probe_offline                                      |                                                                                  | await_reboot, SeenOffline, deadline now+RebootWindow                                                             | probe after ProbeInterval                                           |
| upgrade_issued                         | probe_online                                       | uptime < UptimeBefore or version/image changed                                   | verify                                                                                                           | probe_identity                                                      |
| upgrade_issued                         | probe_online                                       | unchanged                                                                        | upgrade_issued                                                                                                   | probe after ProbeInterval                                           |
| upgrade_issued                         | upgrade_report "Failed ..."                        | Attempt < Max                                                                    | set_ota_url                                                                                                      | set_ota_url                                                         |
| upgrade_issued                         | upgrade_report "Failed ..."                        | Attempt = Max                                                                    | failed                                                                                                           | notify                                                              |
| upgrade_issued                         | timeout                                            | Attempt < Max                                                                    | set_ota_url                                                                                                      | set_ota_url                                                         |
| upgrade_issued                         | timeout                                            | Attempt = Max                                                                    | failed ("device did not start the upgrade")                                                                      | notify                                                              |
| await_reboot                           | probe_offline                                      |                                                                                  | await_reboot                                                                                                     | probe after ProbeInterval                                           |
| await_reboot                           | probe_online                                       |                                                                                  | verify                                                                                                           | probe_identity                                                      |
| await_reboot                           | timeout                                            |                                                                                  | failed ("device did not come back")                                                                              | notify                                                              |
| verify                                 | identity                                           | stage minimal, variant minimal                                                   | set_ota_url, stage final, Attempt 0 (or cancelled if CancelRequested)                                            | set_ota_url                                                         |
| verify                                 | identity                                           | stage final, version = target, variant/lang/family as planned                    | restore_ota_url, or succeeded if restore disabled / nothing to restore / CancelRequested                         | restore_ota_url / notify                                            |
| verify                                 | identity                                           | stage final but device on minimal/safeboot (Tasmota auto-continue, `ota_loader`) | await_reboot, deadline now+RebootWindow (once, recorded in `AutoContinueSeen`; then treat as "old")              | probe after ProbeInterval                                           |
| verify                                 | identity                                           | family or hardware changed                                                       | failed ("hardware changed", critical)                                                                            | notify                                                              |
| verify                                 | identity                                           | still old version (download failed, device kept running)                         | set_ota_url if Attempt < Max, else failed                                                                        | set_ota_url / notify                                                |
| verify                                 | command_failed                                     | before the deadline (shared with `await_reboot`)                                 | await_reboot, deadline unchanged                                                                                 | probe after ProbeInterval                                           |
| verify                                 | command_failed / timeout                           | deadline passed (`timeout` always)                                               | failed ("device is back but its firmware cannot be read: <error>")                                               | notify                                                              |
| restore_ota_url                        | command_ok                                         |                                                                                  | succeeded                                                                                                        | notify                                                              |
| restore_ota_url                        | command_failed / timeout                           |                                                                                  | succeeded, LastError "could not restore OtaUrl"                                                                  | notify                                                              |
| queued, preflight, backup, set_ota_url | cancel                                             |                                                                                  | cancelled                                                                                                        | notify                                                              |
| upgrade_issued, await_reboot, verify   | cancel                                             |                                                                                  | same state, CancelRequested                                                                                      | none (the device is flashing; finish the stage)                     |
| set_ota_url                            | command_ok                                         | `ViaSafeboot`                                                                    | safeboot_switch, deadline now+SafebootWindow (Attempt unchanged)                                                 | safeboot_switch (Guard, one `/u4?u4=fct`)                           |
| safeboot_switch                        | command_ok                                         | device answered `false` or `true`                                                | same state                                                                                                       | probe after ProbeInterval                                           |
| safeboot_switch                        | command_failed                                     | certain: the device did not switch (no factory partition, error status, cancel)  | failed ("device was not touched"), cancelled if CancelRequested                                                  | notify                                                              |
| safeboot_switch                        | command_failed                                     | lost reply (uncertain)                                                           | same state                                                                                                       | probe after ProbeInterval, never `/u4` again                        |
| safeboot_switch                        | probe_offline / probe_online                       | not answering as safeboot                                                        | same state                                                                                                       | probe after ProbeInterval                                           |
| safeboot_switch                        | probe_online                                       | answers as safeboot                                                              | upgrade_issued (Attempt+1, UptimeBefore = safeboot's uptime, deadline now+UpgradeAccept)                         | upgrade (Guard again, `/up` must be the plain form, one upload)     |
| safeboot_switch                        | probe_online                                       | answers as safeboot, CancelRequested                                             | safeboot_recover                                                                                                 | safeboot_back                                                       |
| safeboot_switch                        | timeout                                            |                                                                                  | safeboot_recover (reason "did not answer as safeboot")                                                           | safeboot_back                                                       |
| upgrade_issued (ViaSafeboot)           | command_failed (refused) / guard_refused / timeout | the device is in safeboot                                                        | safeboot_recover                                                                                                 | safeboot_back                                                       |
| await_reboot (ViaSafeboot)             | timeout                                            | never seen offline                                                               | safeboot_recover                                                                                                 | safeboot_back                                                       |
| verify (ViaSafeboot)                   | identity                                           | device is in safeboot                                                            | safeboot_recover                                                                                                 | safeboot_back                                                       |
| any of the above (ViaSafeboot)         | (would enter safeboot_recover)                     | `RecoveryStep` != 0: a recovery ran already                                      | failed ("... a recovery already ran", state unknown)                                                             | notify                                                              |
| safeboot_recover                       | probe_online                                       | answers from safeboot, `RecoveryStep` 1                                          | same state, `RecoveryStep` 2 (stored first), deadline now+SafebootWindow                                         | safeboot_back (the one `/u4?u4=ota`)                                |
| safeboot_recover                       | command_ok / probe_offline / probe_online          | still safeboot or unknown; lost reply; nothing sent yet                          | same state                                                                                                       | probe after ProbeInterval, never `/u4` again                        |
| safeboot_recover                       | probe_online                                       | app answers with the version from before (nothing is sent)                       | failed ("Upload into safeboot failed; the device was switched back to <old>"), cancelled if CancelRequested      | notify                                                              |
| safeboot_recover                       | probe_online                                       | app answers with another version                                                 | verify                                                                                                           | probe_identity                                                      |
| safeboot_recover                       | timeout                                            | `RecoveryStep` 2                                                                 | failed ("Device is still in safeboot at <address>: ...")                                                         | notify                                                              |
| safeboot_recover                       | timeout / command_failed (certain)                 | `RecoveryStep` 1, or the request was refused (HTTP error, 401)                   | failed ("Could not bring the device back from safeboot (...), its state is unknown. Check it at <address>: ...") | notify                                                              |
| safeboot_switch, safeboot_recover      | cancel                                             |                                                                                  | same state, CancelRequested (the recovery ends the job as cancelled)                                             | none                                                                |
| any terminal                           | any                                                |                                                                                  | unchanged                                                                                                        | none                                                                |
| any other (state, event)               |                                                    |                                                                                  | unchanged                                                                                                        | none (logged at debug)                                              |

Refinements (implemented in `machine.go`, tests per row):

- **Never send `Upgrade` into a running or imminent download.** A retry is allowed only
  with reboot proof (uptime < `UptimeBefore`) or after the deadline. Until then the engine
  keeps observing.
- `await_reboot | probe_offline` sets `SeenOffline`. `await_reboot | probe_online` without
  `SeenOffline` and with an old, intermediate or unknown image stays and probes: the
  minimal of `ota_loader` answers ~6 s before it continues, and Tasmota answers between
  download attempts. `await_reboot | timeout`: without `SeenOffline` -> retry
  (`set_ota_url` if Attempt < Max, else failed); with `SeenOffline` -> failed "device did
  not come back".
- `verify | identity` "still old" without reboot proof -> `await_reboot` with
  `SeenOffline=false`.
- Entering auto-continue sets `AutoContinueSeen` and clears `SeenOffline`.
- `Submit` is all-or-nothing: a `*RefusedError` (`errors.Is(err, ErrRefused)`) lists every
  refused device with its reason. `Cancel` on a terminal job -> `ErrNotActive`.
- Approved additions (WP4b): `Engine.Hint(deviceID, report)` feeds MQTT
  `stat/<t>/UPGRADE` reports; `Engine.CancelBatch(batchID)`.

**No power restore** (#551, 0.28.0). The engine never reads or switches relays around an update:
no `Power<n>` command, no Status 11 for relays, no PulseTime/Interlock/ShutterRelay reads, no
`restore_power` state, no "Power after update" choice. What a device does with its relays after
the reboot is its own setting: `SetOption0 1` (save the relay state), `PowerOnState 3` (keep the
last state) and `SaveData 1`. The README says so ("What TasmoFleet changes on your devices"), the
Policy rule template "Restore power after an outage" sets `PowerOnState 3`. Migration 0042 ended
jobs that were waiting in `restore_power` as `succeeded` (with a job event) and deleted the stored
`OTA_POWER_MAX_AGE`; the power columns of `ota_jobs` (0007, 0008) stay unused. The research notes
in `docs/research/tasmota-minimal-and-ota.md` keep the observations that led to the old design.

Resume (process start): non-terminal jobs get `resume`.

| State                             | On resume                                                 |
| --------------------------------- | --------------------------------------------------------- |
| scheduled                         | unchanged: no worker; the scheduler looks at its start (9.4b) |
| queued                            | wait for scheduler                                        |
| preflight, verify                 | probe_identity                                            |
| backup                            | backup                                                    |
| set_ota_url                       | set_ota_url (idempotent)                                  |
| upgrade_issued, await_reboot      | **probe**, deadline = now + RebootWindow. Never `Upgrade` |
| safeboot_switch, safeboot_recover | **probe**, deadline = now + SafebootWindow. Never `/u4`   |
| restore_ota_url                   | restore_ota_url                                           |

### 9.4 Staged ESP8266 flow and the OtaUrl rule

SRC facts: normal firmware persists OtaUrl when `Upgrade` starts. Minimal never persists.
TasmoAdmin's flow therefore left every device with the **minimal URL** stored. tasmofleet:

1. preflight records `OriginalOtaURL` (normal firmware, from `OtaUrl` without parameter).
2. minimal stage: `OtaUrl <fw>/<rel>/tasmota-minimal.bin.gz`, `Upgrade 1`. Stored = minimal URL.
3. final stage on minimal: `OtaUrl <fw>/<rel>/tasmota-<v>.bin.gz` (RAM only), `Upgrade 1`.
4. full firmware boots with stored OtaUrl = minimal URL. `restore_ota_url` sets
   `OriginalOtaURL` back (normal firmware, persisted). `OTA_RESTORE_OTAURL=false` leaves the
   tasmofleet URL instead. This is a deliberate choice with a flash write either way.

Direct flows also restore. If `OriginalOtaURL` was empty or already equal, nothing is
written.

### 9.4a Transport of a job (0.20.0)

The path of an update is chosen once, in the preview, which is also the preflight of Submit: `transport.Choose` over the device's facts (spec "Transport and path choice") gives `Paths.Update`. `None` refuses the device with the chooser's sentence. An update on DownloadMQTT (its commands over MQTT; the device still fetches the image over HTTP) stores `Job.Transport = mqtt`; every device call of the job then runs under `transport.WithPath` (HTTP or MQTT, never rerouted, no failover), also after a restart. Over MQTT:

- The Router builds `RootPage` from the INFO1 seen after the device's latest LWT Online (version, image tag, minimal/safeboot); none gives `ErrBadResponse` (`transport.ErrNoInfo`), so minimal firmware is never recognised on a guess. Since #686 only minimal and safeboot firmware need it (firmware that answered Status 2 reads no page), and after the preflight a job pinned to MQTT reads the INFO1 (`Router.Info1Root`); a job without one (also while another device answers on the topic, T19, #852, when no INFO1 is read) reads the main page over its path, which fails on an MQTT pin, and never goes on from its own chip family alone (#918). A job pinned to HTTP reads the page at the address, never the INFO1 (#810). Outside a job the page at the address is read while the topic is taken. The language comes from the `Status 4` locale over MQTT too; where that names no build the preview asks for it (`Request.Language`, kept as the language of the job's `IdentityBefore`).
- `stat/<t>/UPGRADE` `Failed ... connection failed` ends the stage without a second `Upgrade 1` (`ErrTextFWUnreachable`); the application records FWReach=no.
- The job does not store the identity it probes (it is not an HTTP probe), so the device's GroupTopic read stays fresh for the writes of the job.
- MQTT timings: reply 8 s, connection failure about 66 s after `Upgrade 1`, RebootWindow unchanged. The pre-OTA backup needs the MQTT backup (#257); without it the job fails at the backup unless the backup is switched off.
- The pre-OTA backup of an update over HTTP is not pinned to HTTP (#607): it is a backup like any other and goes over MQTT first when the device is usable there (5.x); every other call of the job stays on the job's path.

### 9.4b Scheduled start (#603)

An update job can wait for a start time (0.36.0). The preview chooses it (#604); the engine runs it.

- `model.JobScheduled` (`scheduled`): not terminal; the job waits for `Job.StartAt` and holds no worker and
  no slot (`OTA_PARALLEL`) until then. Like every non-terminal job it holds its device in
  `ota_jobs_one_active`, and ota's `activeStates` include it: another update, Make compliant, a bulk command
  or backup, a GPIO template and a retry are refused for the device with "has a scheduled update (TX-0010) that
  starts Sat, Nov 7 at 03:00; cancel it first". The person's own writers (console, power, settings writes,
  `app/devicelocks.go`) and the firmware recheck (`app/fwrecheck.go`) do not count it: the device stays usable
  until the start. Restore (`ActiveJobs`) and device delete (the store) are refused while it waits.
- `Job.StartAt`: when the job may start; zero starts at once (every job made before). Fixed at creation,
  never rewritten by a transition and never copied by a retry (`retryRequest`), which starts at once.
- `Submit` with `Request.StartAt`: a time that is not after now is refused as a whole before anything is read
  (`ota.StartInPastError`, `ErrStartInPast`: "Choose a time in the future. It is now Fri, Nov 6 at 10:21.").
  Otherwise the same checks as for an update that starts at once (preview: probe, policy, select, plan,
  Guard, path), and the jobs are stored as `scheduled`.
- The engine (`ota/scheduled.go`): `Run` resumes every non-terminal job except queued and scheduled ones.
  Whenever the scheduler wakes it first looks at the scheduled jobs (`schedule.At(StartAt, now,
  schedule.LateStart)`), then starts queued jobs FIFO as before. It wakes through the engine clock at the
  earliest start or retry, at least once a minute (a timer counts monotonic time, which stands still while the
  host sleeps; the start follows the wall clock).
  - **Wait**: nothing.
  - **Run** (start <= now <= start + 60 min; a start after downtime is late, never early): the device must not
    be excluded from OTA (else `guard_refused`, failed, nothing sent) and must pass `Options.StartCheck`,
    wired in `app/ota.go`: the device lock (taken without waiting, as TasmoFleet's own work) and the firmware
    holds of a typed `Upgrade` (#347), what `lockedOTA.Submit` checks for a person. A refusal is tried again
    every 30 s inside the window. Then, with the lock held, `scheduled -> queued` is stored (event
    `start_due`, detail "planned for Thu, Nov 5 at 03:00, started 2s after it") and the lock is released; from
    then on the active job keeps the writers away. The normal preflight follows: identity, policy, select,
    plan and Guard run again, and Guard again before every `OtaUrl` and `Upgrade` (safety rule 1). The path
    (`Job.Transport`, 9.4a) and the delivery stay those of the plan: a push whose plan no longer allows it
    fails in the preflight as for any job; the path is not chosen again.
  - **Missed** (now > start + 60 min): the job ends `cancelled` with `NotStarted` (event `not_started`,
    LastError "TasmoFleet was down at the planned start (Thu, Nov 5 at 03:00) and was not back within 60
    minutes. It came back at Thu, Nov 5 at 04:12. No device was changed, and this update will not start
    later."). It never starts later. A job whose device this engine found busy until the end of the window
    fails instead (`guard_refused`, the reason in LastError): TasmoFleet was running, so "not started because
    TasmoFleet was down" would be wrong.
  - `Step` checks the window again for each of these events, so an early or late event never starts the
    job outside it and a job that may still start is never written off. The start re-reads the job and
    stores `start_due` (or the end of the job) under the engine lock, as `Cancel` does, so a cancel and a
    start never both act on it; the start check itself runs without the lock (it must not block), and a
    cancel during it wins (the re-read sees `cancelled`).
- `Cancel` and `CancelBatch`: `scheduled -> cancelled` at once (not `NotStarted`); nothing was sent.
- Notification: when no job of the transaction ran and at least one ended `NotStarted` (the others were
  cancelled by the person before the start), the job callback sends `update.not_started` (9.4b, 14.5)
  instead of `update.finished`, counting only the devices that were not started: title "Scheduled update
  not started", text "TX-0008 (Update to Tasmota 16.1.0, 12 devices) was planned for Thu, Nov 5 at 03:00.
  TasmoFleet was down and was not back within 60 minutes, so the update was not started and no device was
  changed. Plan it again to run it." Once, when TasmoFleet is back. It follows `NOTIFY_UPDATE` and is sent
  under `problems` too.
- Metric: `tasmofleet_ota_jobs_active` counts jobs at work, not jobs waiting for their start.
- Dashboard and status bar (#707): both say how many plans wait and when the first starts ("2 scheduled · next
  Sat, Nov 7 at 03:00"), linking to the Scheduled section of Transactions (`/transactions#scheduled`). The count is
  per transaction, like "Scheduled · N" there, not per device. A transaction is a plan while it has `scheduled`
  jobs and none that is running or queued (`planWaits`, the one rule Transactions, the Dashboard and the bar share);
  once some device began it is a running transaction. The line shows a plan from the moment it exists (the
  engine publishes no job event when one is created, so a page that is already open learns of it at its next job
  event or reload) until it is cancelled, started, not started or refused; each of these is a job event, which
  refreshes the Dashboard and the bar. The bar's phone line and its colour level stay as they are: a plan is not a
  problem and not running. Nothing is stored or cached; every render reads the open jobs. A failed read shows
  nothing.
- `Job.NotStarted`: the job ended `cancelled` without starting, because its start had passed by more than
  the late-start window; nothing was sent to the device.
- Migration 0048 adds `ota_jobs.start_at` (milliseconds, like `deadline_at`; 0 = none) and
  `ota_jobs.not_started` (0/1), both `NOT NULL DEFAULT 0`, so rows from before read as "at once" and
  "started". `CreateBatch` and `CreateRetryBatch` store `start_at`; `SaveTransition` writes `not_started`
  and never `start_at`. `store.JobFilter` has `CreatedBy` (the creator's subject) and `Scheduled`
  (`start_at <> 0`, whatever the state).
- `schedule.LateStart` = 60 min, fixed (not a setting; the same window as scheduled backups and snapshots:
  `notify.MissedGrace` is this constant). `schedule.At(start, now, grace)` is pure: `Wait` before `start`, `Run` while
  `now - start <= grace` (the boundary of `schedule.Plan`: 60:00 late runs, 60:01 does not), `Missed`
  after; a zero `start` is `Run`.

### 9.5 Timeouts and limits (defaults, configurable)

| Name           | Default                          | Reason                                                                                                              |
| -------------- | -------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| CommandTimeout | 30 s                             | per command incl. client retries                                                                                    |
| UpgradeAccept  | 3 min                            | download blocks the web server, so "no answer" is expected; OBS end-to-end 20-40 s                                  |
| RebootWindow   | 8 min                            | 10 download attempts on ESP8266 (SRC) plus boot plus Wi-Fi join                                                     |
| ProbeInterval  | 5 s                              |                                                                                                                     |
| SafebootWindow | 2 min                            | ESP32 restart into the other partition plus Wi-Fi join; measured about 6 s (to safeboot) and 11 s (back to the app) |
| MaxAttempts    | 2 per stage                      |                                                                                                                     |
| OTA_PARALLEL   | 2 jobs past `queued`, fleet-wide | ESP8266 web servers and a small firmware server; staged jobs hold their slot for both stages                        |

### 9.6 Concurrency and scheduling

One engine goroutine owns scheduling. Each active job has a worker goroutine that executes
the actions of one transition at a time. The per-device HTTP serialisation in `tasmota`
also covers health polls running at the same time. Batches start FIFO by job id. A batch
can be cancelled as a whole. `Submit` refuses devices with `OTAExcluded`, devices that
already have an active job, and devices whose Preview decision is `skip` or `refuse`
(returned to the caller, not silently dropped).

**Deleting a device while a job holds it (#466).** `ota_jobs` and `restore_jobs` cascade with their device, and
`ota_jobs.id` is reused after a delete (no AUTOINCREMENT), so a worker that still holds a deleted job would find
its row gone or, after id reuse, another device's job in another state. The store therefore refuses the delete:
`DeviceStore.DeleteDevice` returns `store.ErrDeviceBusy` (wraps `ErrConflict`, never `ErrNotFound`) while the device
has a job that is not succeeded, failed or cancelled, or a restore that is not done or failed. The check and the
delete are one statement in one transaction. The delete page says "An update is running on this device" with a link
to the transaction (or the restore) and offers no Delete; a post is answered 409 and audited as refused, and the check runs before "Delete the backups too", so no backup is
deleted; only when a job starts between that check and the delete does the store refuse after the backups went, and the
409 page says so. The engine treats a vanished job row (`store.ErrNotFound` on a persist) as a quiet stop (WARN, no ERROR);
a conflict stays an ERROR.

### 9.7 Preview (dry run)

`Preview` runs read-only probes, Detect, Decide, Select, Plan and Guard (against the URL
the artifact would get) and returns a per-device row: upgrade / reinstall / downgrade /
skip / refuse with reason, plan (direct/staged), file name. The UI always shows it before
Submit.

---

## 10. Health model (package `health`)

| State       | Rule                                                                                                                                                               |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| online      | MQTT device: LWT `Online` and any message within `StaleAfter` or a background poll that reached it within one `POLL_INTERVAL` (+20 %) with no failed poll since (#669), or LWT `Offline` while the background poll reaches it (#619). HTTP-only device: last poll succeeded |
| stale       | MQTT device: LWT `Online` but silent longer than `StaleAfter` (default 2 x TelePeriod + 60 s = 660 s) and no background poll reached it within one `POLL_INTERVAL` (+20 %) either (#669). Triggers an HTTP poll |
| offline     | LWT `Offline` while the background poll does not reach the device, or `OfflineAfter` (3) consecutive failed polls                                                  |
| auth_failed | the device answers but refuses our credentials (401 / `WARNING Need user=`). Never counted as online (TasmoAdmin lesson B7)                                        |
| unknown     | nothing observed since start                                                                                                                                       |

- The HTTP poll uses `Status 11` (cheap; uptime, Wi-Fi, power) and falls back to the `Upgrade`
  probe when the reply is Unknown (minimal firmware, shown as "minimal" in the UI).
- Poll scheduling: HTTP-only devices every `POLL_INTERVAL` (5 min) with ±20 % jitter. MQTT
  devices only while their telemetry is silent (nothing within `StaleAfter`), and then like an HTTP-only
  device: an HTTP answer or a refused API keeps such a device online but does not stop its polls (#652),
  and what the background poll reaches (an answer or a refused API, the snapshot read included, never a probe) keeps
  it fresh for `max(StaleAfter, POLL_INTERVAL + 20 %)` plus 35 s (one tick and the poll timeout), until its next poll
  is due, so it stays online at every `POLL_INTERVAL` (#669). That holds whatever MQTT sent since, as for an HTTP-only
  device: a device whose telemetry stops without its will firing stays online until its next poll. The first
  background poll that does not reach it ends that until a background poll reaches it again: from then on only
  `StaleAfter` since its last contact counts, and an MQTT message or a probe does not bring the window back. MQTT
  contact alone keeps `StaleAfter`, and the poll rate does not change. A device whose last will
  is `Offline` is polled like an HTTP-only device too (#619): the retained will outlives a device that left
  our broker unclean (a power cut while switching broker, a restored config for another broker) or has MQTT
  switched off. A new will (one that follows `Online`) makes the device offline at once and is checked one
  poll interval later (with the jitter), not while the device reboots (#652); the broker's replay of the
  same will after a reconnect changes nothing. Any other replayed will (at start, after a reconnect, for a
  device added later) is checked at a random point within one poll interval, so a start does not poll all of
  them on the first tick; when nothing was known of the device before that will, its first background poll is
  the device's baseline, like a first observation, and writes no history row (#652). Offline devices back off
  exponentially up to 30 min, or `POLL_INTERVAL` when that is longer. At most 4 polls in flight.
- HTTP probe (#456): a device that is fine on MQTT is not polled, so its HTTP fact (`Health.SeenHTTP`)
  only ages. `Service.ProbeHTTP(id)` asks for one deliberate `Status 11` instead, run by the next tick: when
  the device page is opened, and when the broker re-read (#598) is due for a device whose HTTP fact is not
  fresh (one device per pass, a given device at most every 30 min). It is limited per device (not within
  5 min of an HTTP answer or of the last probe, 10 min after a failed one) and not asked for while a job, a
  lock or a typed firmware update holds the device. A probe records what HTTP showed (`SeenHTTP`,
  `HTTPRefusedAt`) but never counts toward offline and does not move the poll schedule. For a device whose
  last will is `Offline` it never moves the device between online and offline either: the background poll alone
  decides that (#619), so opening its page neither brings the device back nor takes it offline.
- `Health.HTTPRefusedSince` (#645): when the HTTP API was first found off in the current run of refusals (in memory;
  cleared together with `HTTPRefusedAt`). The connection card and the skipped-backup note show it as "since";
  `HTTPRefusedAt` keeps moving with every refusal and is what the rules read.
- `Health.HTTPRefusedAt` (#447): when the last HTTP call found the device's web page answering but its HTTP
  API off (`tasmota.ErrHTTPAPIRefused`, SetOption128); zero when HTTP answers or the device does not answer at
  all. The router's memory of the same finding lasts 10 min; the transport facts (`TransportFacts.httpState`) apply
  the tracker's finding after it, so the router, the page and the gates decide alike. A refused call is an answer:
  it counts as contact (`LastSeen`), never toward offline, and never as `SeenHTTP` (the API did not answer). What
  HTTP showed beats an `Offline` state on the page only when it came after the device went offline
  (`Health.Since`); a device kept offline by its last will says in the offline banner what HTTP showed until the
  next background poll, whose answer or refusal makes it online (#619): with MQTT off as well, a device whose
  API is off is online and has no path. `Health.APIOff()` is the one rule for tile, card, list and gates. A
  refused poll waits like a failing one (up to 30 min, or `POLL_INTERVAL` when that is longer), except for a device on our
  broker whose last will is not `Offline`: once its telemetry is silent its refusals are what keeps it online, so its next poll is due at the normal interval (#652), after a refused snapshot read too (#669; the snapshot read itself is not repeated before the back-off). For a device whose last will is `Offline`, only its own MQTT
  messages and what the background poll reaches (an answer or a refusal) count as `LastSeen`: a probe contact
  does not move "Offline since". An online device whose API is off
  and which no MQTT path can serve has no path for backup, update, restore and changes: scheduled backups skip it
  (reason "HTTP API off (SetOption128 0)"), and updates, bulk actions, manual Back up, relay switches, Restart, Refresh identity, Console and Settings posts, transaction retries (backup, command, Make compliant), Make compliant, GPIO template apply and the Drift tab's live read refuse it up front with that reason (#645); the background policy read skips it silently, and so do the on-demand policy check, the GPIO templates refresh and the group-topic reader, which do not count it as a failed read and read it again once the API answers (#659).
- MQTT messages update `LastSeen`, power state and RSSI immediately and push SSE events.
- A software restart publishes LWT `Offline` about 2 s after the command and before every OTA reboot
  (Tasmota's PubSubClient closes the socket without a DISCONNECT packet, so the broker fires the will;
  `docs/research/tasmota-mqtt-transport.md` section 5, live 2026-10-30). Health does not use a
  reboot to judge the device's state; the OTA engine uses uptime. Since 0.25.0 it does notice restarts
  for the stability history (#325): the device's tele/INFO3 (RestartReason, BootCount), or an uptime
  that fell by more than 30 s between two readings followed by one `Status 1` over HTTP (never
  Status 0; three tries, then given up), and once per device the last restart from `Status 1` when
  none is stored. It also stores one Wi-Fi sample per device and clock hour. The verdict (Stable,
  Boot loop: 3 restarts in 1 h or 6 in 24 h, Weak Wi-Fi: average below -75 dBm over 24 h or 10
  reconnects since boot) is the pure package `internal/stability`. Every live INFO3 is one restart (it comes
  once per boot; Tasmota raises BootCount only after 10 s of uptime, so a fast loop repeats it and BootCount
  is no key); BootCount steps only add the restarts TasmoFleet missed, when both neighbouring rows are inside
  the window. After a crash INFO3 carries `RestartReason` as an object, whose `Reason` is taken. A restart
  while an update, restore or command job of the device runs (or ended within 3 minutes), or within 3
  minutes of a command that restarts it (`cmdpolicy.RestartsDevice`: the Restart button, bulk and console
  Restart 1/99, Upgrade, Reset) is stored as
  expected and does not count towards a boot loop, but is still the "last restart". A light behind a wall
  switch that is flicked reports "Power on" and can show as a boot loop of power-ons.
- Only transitions are persisted (`health_events`), plus `devices.health_state/last_seen_at`
  for a correct picture right after a restart. A device's first observation after the app
  started (state "unknown" to anything) is a baseline, not a change: `devices.health_state` is
  written, no `health_events` row (decision 2026-10-23: a restart logged "Unknown to Online"
  for every device). The cost: a change that happened while the app was down is not recorded.
- MQTT input is untrusted and bounded: payload cap, bounded topic grammar, a retained
  replay cache with per-device reservations for inventory devices and a small evicting pool
  for unknown topics, and a fail-closed known-device index. Authoritative:
  `docs/security/mqtt-ingest.md` (attacker model, budgets, claims Q1-Q10, residual risks,
  broker ACL recommendation).
- No broker configured -> the tracker runs HTTP polling for every device. The app
  starts and runs without MQTT.

---

## 11. Inventory, import and discovery

### 11.1 Inventory (M1)

Add/edit/delete by admin. Add needs address (+ port, credentials) and runs a probe
(`Status 2/4/5/6/11`) to fill MAC, topic, name, identity and MQTT capability. The
address must be inside `DEVICE_NETWORKS`.

**Identity rule (2026-10-20).** A device is identified by its MAC addresses.
Matching in discovery, MQTT announcements, import and probes is by MAC first; address,
hostname and topic are mutable attributes. A device stores a set of MACs (Wi-Fi station
plus Ethernet where present: ESP32 boards with Ethernet report one MAC per interface).
The MAC set changes only through an explicit action ("Replace device"), with one exception:
a device stored without any MAC gets the MAC it reports bound by a person's "Refresh identity"
(5, Refresh identity; #65). A MAC is not a
secret: it identifies a device, it does not authenticate it. Consequences: a discovered
device whose MAC is in the inventory is the same device, never a new one; a stored
hostname is never replaced by an IP automatically (a hostname that resolves to the
discovered IP is a no-op); a real address change is offered as "Known device – address
changed" (update by an admin, or dismiss), not applied silently. Device credentials
exist only when a password is set (a web user without a password means no auth).
`OTAExcluded` replaces TasmoAdmin's "updatable" flag. Tags are free text and select
devices in batches.

### 11.2 TasmoAdmin import (M1: CLI; M2: web upload)

`tasmofleet import tasmoadmin <dir>` reads `devices.csv` (fputcsv with `\` escape, 7-16
columns), `MyConfig.json`, and optionally `.device-password.key` (AES-256-GCM `enc:v1:`
passwords; layout `iv(12) || tag(16) || ciphertext`). It maps columns 0-4, 10 (updatable ->
OTAExcluded), 11 (port), 12, 14 (topic). Dashboard columns are dropped. Every row is then
probed (MAC). Duplicate MACs are merged and reported. Rows without an address are
reported. `--dry-run` prints the plan. The local login hash is not imported (local
accounts are created fresh). MyConfig `ota_server_ip/port` is printed as a hint for
`FIRMWARE_BASE_URL`.

### 11.3 Discovery (M2)

- **MQTT discovery**: a continuous subscription to retained `tasmota/discovery/+/config`
  (fields `ip, dn, fn, hn, mac, md, sw, t, ft, ...`, OBS) and, since 0.13.0, to the device
  topics themselves (11.3a). Unknown MACs become proposals.
  A known MAC (any MAC of the device's set) is the same device. At the same place (stored IP,
  or a stored hostname that resolves to the discovered IP) nothing happens, not even an audit
  row. A changed IP of a device without a password is verified by MAC and updated (fixes DHCP
  drift). A stored hostname is never replaced by an IP, and a device with a password is never
  moved by discovery: both become an "address changed" suggestion that an admin applies or
  dismisses. Refused attempts are audited at most once per device and day. Dismissed
  proposals can be listed ("Show ignored") and restored.
- **HTTP autoscan**: an admin starts a scan of CIDRs, each of which must lie inside
  `DEVICE_NETWORKS` (at most 1024 addresses per scan). It probes `Status 2` with 16
  concurrent requests, a 2 s timeout and 1 retry, and streams results over SSE. It runs as a
  cancellable background job. Loopback, link-local, multicast and broadcast addresses are
  always skipped.
- Proposals are added only by explicit admin action.

#### 11.3a MQTT-first discovery (0.13.0, issues #47 and #45, decided 2026-10-26)

**Why.** Until 0.12 MQTT discovery read only the retained `tasmota/discovery/<MAC>/config`.
Tasmota publishes it only with `SetOption19 0` and only in builds with
`USE_TASMOTA_DISCOVERY`; `SetOption19 1` removes it, and self-compiled builds often leave
the driver out. Such devices are on the broker (retained LWT, STATE, INFO) but were never
proposed (reporter: 40 devices on the broker, 24 in TasmoFleet). Second, "Add…" on a
discovered device opened the manual form, which probes over HTTP first: an offline device
(or one that refuses the probe) could only be added "without probing", and then without
the MAC and topic that discovery had seen, so MQTT could never match it afterwards.

**Sources.** With a broker, MQTT is the primary discovery source. HTTP discovery (scan)
stays: it is the only way to find devices without MQTT and freshly flashed ones that have
no broker yet. MQTT discovery listens to (decoded with the configured FullTopic, default
`%prefix%/%topic%/`):

- retained `tasmota/discovery/<MAC>/config`: a full claim (MAC, IP, name, topic), as before;
- `tele/<topic>/LWT` (retained), `tele/<topic>/STATE`, `tele/<topic>/INFO1..3`: the topic
  exists and whether it is online. They carry no MAC, so they cannot become a proposal alone;
- `stat/<topic>/STATUS5`, only for a topic discovery itself asked (below): MAC (Wi-Fi, plus the
  Ethernet MAC if any), IP (the Ethernet one when Wi-Fi has none) and hostname.

**On-demand Status 5.** A topic that is online (LWT `Online`, or a live STATE/INFO) and that
neither an inventory device (by topic) nor a discovery config (by `t`) accounts for gets one
`Status 5` over MQTT (`cmnd/<topic>/STATUS` payload `5`). Never `Status 0` (safety rule 2),
never on a schedule: only while the topic is unresolved, at most once per topic per 10
minutes, one command at a time and at least 1 s apart for the whole bus, never to an
offline topic, and only after the topic has been online for 30 s (Tasmota publishes its
discovery config a few seconds after the LWT; a device that announces itself is not asked).
Each query is logged and audited (`discovery.identify`, topic and reason), and switching
MQTT discovery off (`DISCOVERY_MQTT`) stops them. Reason: Status 5 is the smallest reply that carries the MAC, and the MAC is
the identity (11.1); a device answers it as cheaply as a STATE.

**Unresolved topics.** Topics without a MAC yet (offline, no answer, address outside
`DEVICE_NETWORKS`) are kept in memory (at most 1024, oldest dropped) and listed on the
Discovered box of the Add page under "Seen on MQTT, not identified yet" with the reason, so "on the
broker but not in the list" is visible instead of silent. They cannot be added: there is no
MAC to bind. A claim refused only because its address lies outside `DEVICE_NETWORKS`
(discovery config or Status 5 answer) is kept with its topic and evaluated again as soon as
the device networks change (settings page, live): Tasmota repeats neither on its own, so
without this the device would stay listed until a restart (#45). The re-evaluation sends no
MQTT command. A kept claim older than 1 hour is not used (its address may be stale); the
topic is asked again with Status 5 instead. A newer stored sighting of the same MAC wins
over a kept claim, and a topic that becomes an inventory device drops its claim. After the
device networks shrink, proposals outside them stay listed until they expire (30 days) but
cannot be accepted.

**Lookup.** A device that is on the broker but not listed under New devices is in one of four places:
ignored, in the device list (also when discovery bound it to a stored record without MAC, which keeps
its old name), listed as an address change or conflict, or not identified yet. The Add page answers
this for one device (#713): `GET /devices/new?why=<IP, MAC or topic>` (admin only) expands the text
once through the topic table (an entry whose MAC, IP or topic it is gives all three, newest sighting
first), looks the record up in the inventory with the same `match` order discovery itself uses (MAC,
topic, address), and lists the stored proposals (open and ignored, as the page shows them) and the
unresolved topics that belong to it. The answer links to the device, offers Restore with the version
shown, or gives the reason of the topic. It stores nothing, and it sends nothing to a device or the
broker; it covers the device list, the topics seen since the start and the proposals of the last 30
days. "Show ignored" says when an ignored device comes back: when what it announces changes, or when
it announces itself after 30 days without a sighting.

**One device, two sources.** Both sources resolve to one proposal by MAC (the proposal row
is unique per MAC). For a device seen on MQTT the topic, full topic and online state come
from MQTT: a later scan sighting of the same MAC keeps them (and `via` stays `mqtt`) and
only contributes the HTTP port it found; an MQTT sighting keeps the port a scan found. The
online state shown with a proposal is the topic's last LWT.

**Adding without a probe.** Each discovered row has a direct "Add" that accepts the
proposal as seen (MAC, topic, full topic, address, port; `discovery.Accept`). It does not
depend on an HTTP probe: for a proposal whose LWT is `Offline` the MAC check is skipped (it
could only time out); otherwise an unanswered check is fine as before. With the fleet
default password the MAC must still be proven (security review 2026-10-22), so an offline
device is then added without it (audited as `default_password_withheld`); no first probe is
started while it is offline. "Add…" (the prefilled form) stays for typing a password.

**Filled in later.** A device added this way has no identity yet. When the topic of any
inventory device without a detected identity comes online (LWT `Online`, or a live STATE),
discovery starts the first probe again (the same hook as after an accept), at most once per
device per 10 minutes and not again right after an accept already started it. The trigger
is an MQTT message anyone can publish and the probe carries the stored credentials, so
the stored address is first asked without credentials (D15): another MAC refuses the
probe; for a device with a stored MAC and a stored password, an address that cannot prove
the MAC does not get the password on this trigger (retried every 10 minutes); without a
password the probe runs. A device without a stored MAC (for example a TasmoAdmin import
whose first probe failed) is probed at the address the admin bound with its password; the
probe never binds a MAC, the audit records the MAC that answered. Queries and probes
pause while the broker is unreachable. Topic and MAC of a stored device are never changed
from MQTT (D2).

No migration, no store contract change and no new dependency.

---

## 12. Backups (package `backup`)

### 12.1 What is stored per backup run

1. **Raw `.dmp`** from `GET /dl` (Basic auth, correct port). This is authoritative and the
   only input for restore.
2. **Snapshot JSON** built from `Status 1..11` (one by one, never Status 0) plus `Template`,
   `Module`, `Rule1..3`, `Timers`. Read-only reference.
3. **Decoded JSON** from `decode-config` when `DECODER_PATH` is set and the decoder
   version ≥ the device firmware version. It is stamped with the decoder version. If the
   device is newer, only 1+2 are stored and the backup is flagged "decoder outdated".

4. **Files archive** (0.26.0, #324) `<stamp>_<host>.files.tar`, kind `files`, for an ESP32 or any build whose
   firmware is >= 14.6.0 and that has a flash file system (`tasmota.FilesSupported`: `UfsType` 2 or 3).
   It holds every file Tasmota's own file manager shows on the flash file system (the SD card is never
   read), `/tlskey` included, within the limits below. Hidden dot files (`/.settings*`, `/.drvset*`) are
   not listed by the device and not needed: they travel in the `.dmp`. The first tar entry is
   `.tasmofleet-manifest.json` (`fs_type`, `fs_size_kb`, `fs_free_kb`, `files[]` = stored files with
   `path` (device path, leading slash), `size`, `sha256`, `stored`, `mod_time` (device clock, no zone),
   `private_key`; `skipped[]` = listed but not stored with `path`, `size`, `reason`; `via` = `mqtt` or `http`, the
   transport of the walk, absent in archives from before #924, which were read over HTTP); the other entries
   are the stored files under their device path without the leading slash, with the device's mtime.
   Limits: 1 MiB per file, 4 MiB stored per run, 100 files read, 100 folders listed, folder depth 4.
   A file over a limit is in `skipped`, not in the tar. Files are best effort: a file that cannot be read,
   an unlistable folder or a device that stops answering never fails the run; the archive row's note says
   `files incomplete: N files not stored`, and after a lost link nothing further is requested (every call
   would wait for its own timeout). The walk runs under its own deadline, up to 5 s before the caller's
   (the pre-OTA backup has 30 s for everything): what is not read by then is `skipped` with "time limit of
   the backup reached" and the dump is kept. Each download is limited to the remaining run budget. A tar
   that cannot be written or indexed is logged and noted as `files_error` in the audit detail; the run
   still succeeds. Without an archive the run says why in the last sentence of the dump's
   note, `files not backed up: no filesystem | firmware older than 14.6.0 | they need HTTP | <failure>`
   (`transport.Router`'s `tasmota.FileSystem` calls go to HTTP only, also under a pin to MQTT, and answer
   `ErrFilesNeedHTTP` without a request while the device's HTTP is known to fail: "they need HTTP"). After a dump
   over MQTT (#607) a device without an SD card is read on its topic first (#924): `UfsList` per folder through
   `Router.MQTTListFiles` and `FileDownload {"Password":…,"Type":8,"File":"<path>"}` per file through
   `Router.MQTTDownloadFile` (firmware built with `USE_UFILESYS_MQTT`, arendst/Tasmota#25278), under the rule of the
   dump's `FileDownload`, with no identity question and no HTTP request; a folder's files are read before its
   subfolders. A stock firmware answers the first file start that is published with Error 3 (before it sends
   anything of it; a file refused before anything was published does not count): the walk ends there, and the files are read over HTTP as below. A device with an SD card goes there at once (Type 8 and
   `UfsList` would reach the card). Over HTTP the files are read only when the host at the device's address answers
   `Status 5` over HTTP with one of the device's MACs ("the device at its address is not this device" otherwise);
   when HTTP fails after MQTT could not carry the files the reason names both: `they need HTTP (this firmware sends
   no files over MQTT | an SD card is mounted), and HTTP could not be used: <why>`. No migration:
   `backups.kind` is free text, and a run is still all rows with one `TakenAt`, so retention, Keep, delete
   and the orphan handling cover the archive unchanged. Order inside a run: dump, snapshot, `UfsType`,
   `UfsList` per folder (name order), one `GET /ufsd` per file.

   **Files fold on the Backups tab.** A run row with an archive has a native `details` fold ("Files",
   summary "n stored · size · m skipped"; keyboard: Enter/Space). Inside: the stored files with their size,
   then the skipped ones with the reason; `Changed` / `New` compare each file's SHA-256 with the manifest of
   the newest earlier run that has an archive (runs without one, or with a damaged one, are skipped; the first
   run with files has no badges); `tlskey` carries "Private key"; the filesystem line (which names the transport
   that read the files, "read over MQTT" or "read over HTTP") and a sentence that
   contents are never shown and the archive is as secret as the settings dump. Admins get "Download files
   (.tar)", the existing audited `GET /backups/{id}` with the archive row's id (`kind: files` in the audit
   detail); operators see names and sizes only. A damaged or missing archive shows "archive damaged" / "archive
   file missing" in the fold and never fails the page. A run without an archive says why in the row's detail
   line (no filesystem, firmware older than 14.6.0, needs HTTP); a run with neither archive nor note, when an
   earlier run of the device has one, says "no files were stored for this run" (the files step crashed
   between the rows), a run before every archive says nothing.

   Read API for the UI and for restore (`*backup.Manager`, not part of `Service`):
   `RunFiles(ctx, id) (RunFiles, error)` (archive row, manifest, or `Note` = why not; any file id of the
   run names it), `OpenFilesArchive(ctx, id) (*FilesArchive, error)` (`ErrNoFilesArchive`, `ErrIntegrity`,
   `ErrBadArchive`; `Manifest`, `Read(devicePath)`), `ParseFilesArchive([]byte)` (strict: manifest first,
   every path through `tasmota.CheckFSPath`, size and SHA-256 per file, nothing unlisted, regular entries
   only), `FilesNoteOf(dumpNote)`, `IsPrivateKey(devicePath)` (exactly `/tlskey`; set again on reading,
   whatever the manifest says).

Files are `0600`, written via temp file + rename, and SHA-256 indexed.

Over MQTT (0.20.0, #257) the raw `.dmp` comes from `FileDownload {"Password":…,"Type":2}` instead of
`/dl` and must be byte-identical. `mqttcmd.Download` follows the live-tested protocol: `FileDownload 0`
first when the state of the topic is unknown (process start or after a bad transfer), then the start
command, answered by `Started` and the header `{File,Id,Type,Size}` (no `?` after `Started`), then exactly
one `?` per Data message until `{Id,Md5}`. Id must stay constant, Size is 2..16 KiB, the received bytes
must equal Size and hash to the MD5; an empty Data, a timeout (8 s per message, 30 s overall) or any
other irregularity ends with `FileDownload 0` and an error, with no retry. An empty Data is typed
(`mqttcmd.ErrEmptyData`, #887): the firmware sends it only when it cannot allocate the ~4 kB block it
needs to build the chunk, so the backup says that the device ran short of contiguous memory and that a
restart usually helps (`backup.LowMemoryError`). `Error 1` is the typed
`ErrPasswordRefused`. Messages are never logged and never put into errors: the start command carries the
password and a minimal firmware echoes it back as `{"Command":"Unknown"}`. The password per device is
set on the edit form (admin; mode not set / broker / own; write-only, blank keeps, saving clears the
refused mark) and stored sealed (D6). `transport.Router.DownloadConfig` uses it when `Paths.Backup` is
MQTT (MQTT first since #607, or pinned to it): own mode sends the sealed value, broker mode the live `MQTT_PASSWORD`;
`Error 1` writes `mqtt_password_bad_at`; a started transfer never falls back to HTTP (only one that provably
published nothing goes over HTTP once, while HTTP is not failing); a backup neither transport can carry fails with a
`BackupBlockedError` whose reason reaches the device page and the backup history.

`mqttcmd.DownloadFile` (#924) runs the same protocol for one file of the file system (Type 8 with `File`): the path
obeys `tasmota.CheckFSPath` or nothing is published; the header's `File` must be the path without its leading slash
(the firmware's form); a Size over the caller's limit ends the transfer with `FileDownload 0` before the first `?`
(`tasmota.ErrFileTooLarge`); Error 3 in answer to the start is `ErrNoFileTransfer` (a firmware without
`USE_UFILESYS_MQTT`), Error 2 `ErrFileUnavailable`; the whole transfer has 30 s or 8 s plus 1 s per 2982-byte chunk
of the announced size, whichever is longer.

### 12.2 decode-config integration

- License LGPL-3.0. Shipped in the image as the unmodified upstream PyInstaller binary
  (v16.0.0, x86_64, SHA-256 pinned in the Dockerfile) and executed as a separate process,
  never linked. Documented in `NOTICE`.
- Decision: ship it in the image (+25 MB plus libz) instead of a Python layer (+50-100 MB).
  `--build-arg WITH_DECODER=false` builds a slim image. On arm64 there is no upstream
  binary, so the decoder is off.
- Invocation: `decode-config --source <tmpfile.dmp> --output-format json` (exact flags are
  pinned by a test against the real binary in integration tests; CI installs the pinned binary
  and sets `REQUIRE_DECODER=1`, so these tests cannot silently skip). Timeout 20 s, `TMPDIR=/data/tmp`,
  empty environment except `TMPDIR`/`PATH`, no network needed.
- `header.env` (platform, kernel and Python of the decoding host) is removed before the
  decoded JSON is stored.
- `Decoder.Version()` parses `decode-config v16.0.0.0 [...]`. It is shown on `/about` and in
  `tasmofleet_build_info{decoder=...}`.

### 12.3 Secrets in backups

`.dmp` and decoded JSON contain Wi-Fi passwords, MQTT password and WebPassword; the files archive
holds `/tlskey` (a private key) and Berry scripts that routinely embed tokens, and is handled exactly
like the `.dmp` (no content viewer; the UI shows names, sizes and "private key" for `/tlskey`). So:
backup files are 0600, downloads need the admin role and are audited, and the UI never
shows the decoded secrets. The diff/drift view redacts every setting with a key that the one secret rule
(`cmdpolicy.SecretName`, #809) names, the same rule that redacts a command in the console: passwords, tokens,
keys, the network names and the MQTT login. TLS fingerprints are compared: a SHA-1 of a server's public
certificate is no secret. Backups do not go into logs or metrics.

### 12.4 Schedules, retention, restore

- Manual (device page, Backups tab, bulk "Back up") and automatic before every OTA (job state `backup`, skippable
  per batch).
- Manual backups have a cooldown of 1 minute per device (HTTP 429 "Try again in ..."); deleting the last backup of a device ends it.
  Every backup reads the device config over its single-threaded web server, so repeated
  clicks must not hammer an ESP8266. Pre-OTA and scheduled backups are not limited. Optional schedule on chosen weekdays at a time of day (`BACKUP_DAYS`, `BACKUP_TIME`; default off, §12.6).
- Retention (`backup.Prunable`, the one rule pruning and the "Pruned at next run" label share): per
  device the newest `BACKUP_KEEP` (10) runs stay, plus every run younger than `BACKUP_MAX_AGE` (30 days,
  `720h`, so repeated manual runs cannot evict the restore points), plus runs pinned with Keep (an
  operator action; kept runs do not count against `BACKUP_KEEP`). Older runs are deleted after a
  successful new backup, never otherwise. A run is the set of files of one backup. A backup that
  failed stores nothing; the attempt is listed as a "Failed" row with Retry (`backup_attempts`).
  When the device answered the failed dump (a transfer fault over MQTT that is not a timeout, an unusable
  HTTP answer), the run reads its free heap and uptime with one `Status 11` over the dump's transport, never
  retried, and stores them with the attempt (`heap_kb`, `uptime_sec`, 0 = not read, #887); the row shows
  them as "free heap afterwards N kB · up …".
- M2 diff view: decoded JSON of two backups, redacted, rendered side by side ("what changed
  since the last backup"). Operators see the redacted values too, except free-text settings (rules, scripts, Berry code, `mems`, `rf_code`), of which they only see that they changed; admins see those as well.
- M2 restore: admin picks a backup. Over HTTP tasmofleet does what the web UI's "Restore configuration"
  does (#430, 0.26.0): `GET /rs` sets the device's upload type to settings, then the multipart
  `POST /u2?fsz=<size>` carries the `.dmp` (field `u2`), both in one device slot of the HTTP client
  (`tasmota.ConfigRestorer`, `Router.RestoreConfig`, HTTP only). Nothing is posted without the `/rs`
  form (a `POST /u2` without the type says "Successful" and restarts with nothing applied; minimal has
  no `/rs`). The success page (reload script, any language) means the device applies it and restarts;
  "Upload Failed" is certain (nothing applied); anything else is an unknown outcome checked on the
  device, never sent again. Every TasmoFleet upload sends its own type-setting GET first (`/up`, `/rs`,
  `/ufsd`), so a type left armed on the device never turns one upload into another. Audited. Until
  0.26.0 the HTTP restore sent `WebGetConfig` with a one-time token on the firmware listener; no
  official build has `WebGetConfig`, so that never worked and is gone (no token, no `/restore/` route,
  no `FIRMWARE_BASE_URL` needed).
- Restore over MQTT (0.20.0, D8/D12; MQTT first since #607): `transport.Choose` decides per restore
  (`Paths.Restore`): MQTT (FileUpload Type 2) for a device usable on our broker for a transfer (the strict rule, a
  usable MQTT password), also while HTTP works; else HTTP (restore upload) while HTTP is not failing; otherwise the
  restore is refused with the reason. A backup that changes the device's MQTT connection (any setting a write counts
  as a cut-off: topics, prefixes, broker, login, client ID, TLS and fingerprints, MQTT off), or cannot be compared, asks
  the lockout question only while the device's HTTP API does not answer `Status 5` with its MAC, at the start and
  right before the upload; after such an upload the device's next commands go over HTTP for a while, and the wait for
  the restart also asks HTTP when the old topic is silent (restore.md R28, R36). `restore.New` needs one
  of the two (`ErrNotConfigured` when neither exists); Replace device needs the HTTP restore upload. The
  order is the same on both paths: safety backup (through the same path), identity (Status 5), the boot
  count (`Status 1`), then the upload. Over MQTT the device applies the dump only after the MD5 matched,
  so an upload that fails or is aborted leaves the settings unchanged. Restart detection (both paths): the
  boot count from a fresh `Status 1` must rise and `Status 11` must show an uptime shorter than the time
  since the upload began, within `RebootTimeout` (3 minutes); a restore does not reset the boot count (it is
  in the first 16 bytes of the settings, which `SettingsConfigRestore` skips). A lost "Done" is an unknown
  outcome and is checked on the device, never uploaded again. Error 1 marks the device's MQTT password bad. The job records its transport and the
  confirmation page names the path.
- Restore and replace put the files back (0.26.0, #324). When the backup run holds a files archive
  (§12.1), a restore or replace writes its files to the device's flash file system **before** the settings,
  so the one restart of the settings restore already loads `autoexec.be` and the Berry scripts. A new step
  ("Write the files", job state `files`, step 2 of 5) sits between the safety backup and the upload; a
  backup without files, or a restore that cannot write them, passes it at once. Rules:
  - HTTP only: the stock firmware has no file transfer over MQTT (Type 8 of `FileUpload` needs `USE_UFILESYS_MQTT`;
    the restore's upload stays on HTTP, #875). A restore routed over MQTT writes the files over HTTP
    too (#607), to the address resolved at the start, after the identity was checked there over HTTP; when HTTP
    cannot be used (no single address, HTTP known to fail or silent, another MAC) it puts back the settings only.
    A device without a flash file system and firmware older than 14.6.0 put back the settings only; the
    confirmation page says so before the person confirms (`restore.Assessment.Files.Why`), and so does the
    finished job.
    A damaged or missing archive is the same: settings only, said on the page. The device is asked
    (`UfsType`, `Ufs`, `Status 2`) only for a backup that has files.
  - Nothing on the device is deleted. Only files in the archive are written; files the device has and the
    archive lacks, and files over the size limits (listed in the manifest, not stored), stay as they are.
  - The identity (Status 5) is checked right before the first file, as before the upload: files of this
    device never go to another one.
  - Per file, one after the other: a name the device would cut (`tasmota.CheckUploadPath`: folder, slash
    and name within 49 bytes, so a root name has at most 47) is skipped before anything is read; read the
    device's current file first (`GET /ufsd?download=`, at most 1 MiB; a file that cannot be read for another
    reason is not overwritten and ends the step); a file whose content already equals the archive's is not
    written (no flash write). **A file whose current content differs is overwritten only when the backup
    taken just before holds exactly that content** (its manifest has the path with the same SHA-256): the
    safety backup caps files at 1 MiB, can be incomplete or have no files, and a restore must be undoable.
    Otherwise the file is skipped with "not in the backup taken before, so overwriting it could not be
    undone", counted in the summary, and the job is not "verified". Then `UploadFile` (GET `/ufsd` and POST
    `/ufsu` under one lock hold), then the file's size is checked with `UfsList` (the device says
    "Successful" after a short write). An error with `tasmota.ErrNotSent` (the POST never started: folder
    missing, link down while the folder was selected) leaves the old file untouched: no put-back, no flash
    write counted. Every other upload is a flash write on the device (`SettingsSave`): the uploads are never
    repeated in a loop, and the job counts them (put-backs included) in its summary.
  - The confirmation is bound to the plan it showed: the form carries the number of files written first
    (`files`, `StartOptions.ConfirmedFiles`); files are written only when the plan at run time has exactly
    that many, so a page that showed no files can never become a restore that writes files.
  - A folder that does not exist on the device (a fresh replacement has no `/lib`) is provable only by the
    upload's GET (`ErrFolderNotFound`; a listing answers "Done" for it): the file is skipped with
    "folder missing on the device; create it in the device's file manager", the restore goes on. No
    binary-safe folder creation exists (`/ufse` rewrites CR/LF).
  - A failed upload (or a wrong size afterwards) can have cost the file, because the device deletes the old
    one first. The old file read before is uploaded again, once, and that put-back survives a cancelled
    restore (it runs on a context that is not cancelled, bounded to 2 minutes); if it fails too the job says
    "FILE LOST: <name>" and names the automatic backup taken before. A file that did not exist before may be
    left partly written (reported, not deleted). Any failure ends the restore as "files partly written,
    settings unchanged" (`failFilesPartly`; "the files could not be written, the device is unchanged" before
    the first write): files already written keep the backup's content, the settings are not sent, the page
    offers the rollback to the backup taken first only after a write. A failure of a later step before the
    settings go over (identity, unreachable, restore upload refused) after files were written says "files
    written, settings unchanged" (`fo.afterFiles`), never "nothing was changed", and offers the rollback too.
  - Verification adds the files: the fresh backup's manifest must hold the written (or already identical)
    files with the archive's SHA-256. A file the device rewrote itself (Zigbee data) is named as differing,
    a file the fresh backup could not read back (over the size limit, unreadable) is named separately as
    "could not be read back"; both make the job "not verified"; contents are never shown.
  - The finished job keeps what the files step did as the first line of its Detail, separated from the
    verification sentence by a newline (`restore.SplitDetail`); the per-file progress is held in memory for
    the latest jobs only (`Manager.FilesProgress`) and is gone after a restart of TasmoFleet. A job that is
    interrupted in the `files` state is an unfinished job like the others: it blocks a second restore of the
    device and the start-up recovery fails it.
  - Audit: `restore.finish` carries counts and file names of differences, never file contents. Contents are
    secret like the `.dmp` (§12.3): not in logs, job details, audit records or pages.

---

### 12.5 Fleet runs: tried, failed, skipped (#409)

A scheduled run (`BACKUP_DAYS`, `BACKUP_TIME`) records what it did per device; the record is stored
(`backup_runs`, `backup_run_items`) so the status bar, the dashboard and the Devices, Backups page
show the last run after a restart. Outcomes per device: `ok`, `failed`, `skipped`. **Tried = ok + failed.**
A skipped device was not tried; skipped devices are counted apart, never colour the status bar and
never raise a notification. The rule:

- persisted `last_seen_at` is zero (and the tracker has not heard the device either): skipped, "never answered";
- offline: skipped, reason "offline" with the time it went offline stored apart (`since_at`, migration 0044; the pages word it "Offline since <time>"). The tracker's verdict decides; right after a start
  it has none (`unknown` for every device until it is heard), and the persisted `health_state` decides
  then: persisted offline is skipped (since = the persisted `last_seen_at`), anything else is
  **tried** (a run right after a restart must not skip the fleet, and must not count the devices that
  were offline before as failed);
- `auth_failed`: tried, and failed when the device refuses (it is reachable and needs action);
- another operation holds the device (its lock, a running transaction, or a firmware update that a
  typed command or another tool started and that is still flashing, #347 and #504; the device guard):
  skipped, "another operation was running". A guard that cannot tell (its database read failed) counts
  as busy: the device is left alone;
- a device deleted before its turn is neither listed nor failed.

`devices.last_seen_at` is written only with a time the device was heard, or refused our password
(that is an answer: the device is there and needs the person); `SetLastSeen` keeps the stored value for
a zero time, so "never answered" stays true for a device that was only judged offline.

The run takes the device guard (owner `backup`) for each device while it backs it up. The notification lists the failed devices only, one line each with its address and the reason the history shows (#685). The run record is pruned with `AUDIT_RETENTION`; a
device's item keeps its name when the device is deleted. Model: `model.BackupRun`, `BackupRunItem`,
`BackupOutcome`, `BackupDeviceRow`; the web layer reads them through `web.BackupFleet` (`LastRun`,
`Overview`, `PruneNext`, `LastMissed`; `Overview` does no device I/O).

### 12.6 The backup schedule (#601, #612)

The scheduled run happens on chosen weekdays (`BACKUP_DAYS`, `mon`..`sun`; none = manual only) at a
time of day (`BACKUP_TIME`, `HH:MM`) on the server's clock (`TZ`). The pure rule lives in
`internal/schedule` and is shared by the timed jobs (the scheduled scan uses it later, #326): the next
and the previous run on the wall clock, so 03:00 stays 03:00 across daylight saving changes. A time
of day that does not exist on the day the clocks go forward (02:30) runs shifted (03:30); one that
exists twice on the day they go back runs once, at its first occurrence.

- **No run at start, none on a settings change.** The schedule asks `schedule.Plan` what is due, given
  the newest thing that settled a scheduled time (`BackupRunTimes`: the start of the newest completed
  row of `backup_runs`). A run settles it; so does a skip (trigger `missed`); so does a change of the
  schedule or the migration that created it (trigger `rescheduled`, written by the loop when
  `BACKUP_DAYS` or `BACKUP_TIME` change, and by migration 0045). A restart settles nothing.
  A schedule with no such row at all begins now instead of making up the past.
- **A change of the clock is a change of the schedule (#884).** The run record holds instants, not the
  clock that settled them. The loop therefore keeps the clock it read the schedule on (`schedule.ZoneKey`:
  the UTC offsets of the location on 1 January and 1 July, so a daylight-saving switch and two zones with
  the same clock do not count) in the `settings` table (`schedule-zone:backup`, `schedule-zone:snapshot`,
  one row per loop, replaced in place; no migration). When it differs at a look, the loop writes the
  `rescheduled` marker first and stores the new key after it: no run, no skip, no notification, as after
  saving the schedule. The first start with no stored key stores it and changes nothing, so the late start
  after downtime stays. An unreadable key means no run until the next look.
- **A missed run** (TasmoFleet was down at the time) starts late when TasmoFleet is back within 60
  minutes of it. After that it is skipped: one `missed` marker, one notification (the scheduled-backup
  event `backup.scheduled_failed` under `NOTIFY_BACKUP`, text "Scheduled backup skipped"), and Devices,
  Backups says so under Last run until a newer run. After a long downtime only the newest time of day
  counts; there is never one run per missed day. A run cut short by a shutdown settles nothing and is
  started again within the 60 minutes.
- The per-device rules of §12.5 are unchanged: the scheduler only starts the run.
- `BACKUP_INTERVAL` is gone (no alias). Migration 0045 turns a stored non-zero interval into every day
  at 03:00 and deletes the row; a stored 0 stays manual only.

### 12.7 The snapshot schedule (#602)

The scheduled database snapshot (System, Snapshots) uses the same rules as the backup schedule: weekdays
(`SNAPSHOT_DAYS`, none = off) at a time of day (`SNAPSHOT_TIME`, default `03:00`) on the server's clock,
`internal/schedule` for the next run and the DST rules, and the same loop (`scheduleLoop` in `internal/app`)
with the same decisions: no snapshot at start or on a settings change, late within 60 minutes, else skipped
with one notification. The skip is the snapshot failure event `snapshot.failed` under `NOTIFY_SNAPSHOT` (title
"Scheduled database snapshot skipped", link to System, Snapshots).

- **The record** is the table `snapshot_runs` (migration 0046: `trigger`, `started_at`), not the snapshot
  files: `SNAPSHOT_KEEP` prunes files, an admin deletes them, and a failed attempt leaves none. A row is
  written for each scheduled attempt (`scheduled`, taken or failed: a failing disk is tried at the next
  scheduled time, not in a loop), each skip (`missed`) and each change of the schedule or the migration that
  created it (`rescheduled`). The newest row of any trigger settles the scheduled times up to it. The rows are
  pruned with `AUDIT_RETENTION`; the loop keeps the newest settled time in memory so that the prune cannot
  make a schedule forget it ran. An attempt cut short by a shutdown is not recorded.
- **Create snapshot now** (#921, admin, System, Snapshots) uses the same copy, name and `SNAPSHOT_KEEP`
  retention through `snapshot.Manager.TakeNow`. It writes no `snapshot_runs` row, because a row would settle a
  scheduled time that is still due. It does not change the scheduled status (a scheduled failure stays shown)
  and sends no notification. It is audited as `snapshot.create` by the person. A newer stored file ends the
  skipped notice.
- `SNAPSHOT_INTERVAL` is gone (no alias). Migration 0046 turns a stored non-zero interval into every day at
  03:00, stores a `rescheduled` marker (so the first start after the update takes no snapshot), and deletes the
  row; a stored 0 stays off.

## 13. Device page, console and settings (M2)

- **Device list with live state**: health, version, variant, family, IP, uptime, RSSI, power
  state(s) with a toggle (`Power<n> TOGGLE`, operator role), filters by tag, health and
  family. Updated via SSE, no browser polling.
- **Console**: sends one command line to one device and shows the JSON reply. `cmdpolicy.Authorize`
  classifies every line: queries and harmless actions (`Power`, `Restart 1`, `Status N`, the example
  list `cmdpolicy.DefaultOperator`) need the operator role, everything else the admin role (admins
  may send any Tasmota command, decision 2026-10-21), a malformed line is never sent. Destructive
  or lock-out commands (`Reset`, `WifiConfig`, firmware commands, ...) get exactly one confirmation
  that names the consequence; arguments of credential commands are never logged or audited.
  Every command is audited, with the transport that carried it (`via`). Replies are shown as escaped text.
  `FileDownload`, `FileUpload` and `Upload` are refused for every role (their payload carries the device's
  MQTT password). On a device tasmofleet reaches over MQTT only, a change of Topic, FullTopic, GroupTopic,
  MqttHost/Port/User/Password/Client or `SetOption3 0` gets the confirmation with the consequence
  "this device is reached over MQTT only; this change can cut TasmoFleet off from it". Over MQTT the commands
  that run other commands (see 6.5) are refused. Claims: docs/security/command-policy.md.
- **Settings editing** (admin, device Settings tab): typed forms for a curated set (FriendlyName1..4,
  Hostname, Topic, FullTopic, TelePeriod, PowerOnState, SaveData, SetOption0/3/4/19 shown with names,
  MQTT host/port/user/password, NtpServer1..3, Timezone/TimeDst/TimeStd). Every field has a validator. Values are never
  concatenated into `Backlog` strings: each field becomes its own command, and `;` in a
  value is rejected (TasmoAdmin lesson B3). Secret fields (passwords) are write-only.
- **Read-back of writes** (#947): after the Settings tab's writes and after Make compliant, every written value is
  read back from the device and compared (`settings.ReadBack`, one home for both): one bare query or one `Status N`
  at a time through the router (MQTT first), never `Status 0`, and only from a device the identity check confirms
  over the transports those reads used (`settings.Manager.ReadVerified`, the rule of #662/#810): a value another
  device answered never counts as read back and never reaches the policy's live values. A secret
  (`cmdpolicy.SecretName`, #809: MqttPassword and MqttUser) is never queried: the tab says "Set, secret, not read
  back". When a written value restarts the device (`settings.RestartsDevice`) the first read waits 5 s: Tasmota
  restarts on the once-a-second tick after its backlog is empty and saves every setting first (support_tasmota.ino
  16.0.0, lines 1569-1641), so a first read that still meets the device before its restart reports what it reports
  after it; a read the device does not answer is tried again (six attempts, 5 s apart), and a value that differs is
  read again until it settles (#559, about one second each, six times). The comparison is `settings.Matches`, which
  the policy's live check uses too: whitespace, ON/OFF as 1/0, coordinates at four decimals and Timezone in the
  firmware's own `+HH:MM` form (`CmndTimezone` answers `Timezone 1` with `+01:00`). The tab shows a "Read back"
  column (Verified; Differs with the device's value; Not reported; Not read back with the reason); the result is
  part of the one POST response and kept nowhere else, bounded by the read timeout as a whole and ended by
  TasmoFleet's stop. Make compliant (coordinator's decision of 2026-11-17): a value read back from the confirmed
  device that differs is policy drift, not a failure: `policy.DriftError` carries the check of every line, the job
  succeeds, its done event keeps the checks as JSON (`{"note","read_back"}`; a job without drift keeps its plain
  note), the card marks each value (Verified, Differs with the device's value, Not sent for a line left out) and
  the audit `policy.fix` names the settings that differ (`differs`, names only). A value hidden by
  `cmdpolicy.Redacted` keeps the device's value hidden too. A read that fails, a device that cannot be confirmed
  or a value the device does not report fails the job as before (outcome unknown). A secret a rule sets (MqttUser)
  is never read back: it counts as set, neither as a failure nor as drift, and the card says "Set, secret, not read
  back". It counts as set when its send was answered. A restarting Backlog whose send gets no answer (unreachable or
  timed out) goes on to the read-back only when its chunk holds a value the read-back can read (`settings.ReadsBack`);
  a chunk of secrets alone fails the job with the send's error, as an unknown outcome. The read-back decides about
  that chunk's readable values only: the secret in it is not confirmed by them, so if such a value reads back
  different the job shows drift and the secret is still shown as set. The
  policy page's value of it is not refreshed by the read-back: over MQTT the device's own reply to the
  write (`{"MqttUser":…}`, which the live listener stores) updates it; over HTTP the page keeps the value it last
  read until the next read of the device.
- **Last known values** (#599): the Settings tab and the Policy GPIO templates tab draw what was read before at
  once, with its age ("Read 3m ago"), and read the device in the background when they open; the page never
  waits for the device. The values live in memory only (no table, no migration): after a restart the first open
  shows the loading state and reads. Settings: `settings.Manager` remembers the last complete `Read` per device
  (`Last`) and runs one `Read` per device at a time; a second open while one runs waits for it and sends nothing
  (the client already serialises requests per device, so this keeps a second round of about thirty queries from
  queueing behind the first). The read runs detached from the first caller's request, so closing the page does
  not fail the readers that joined. A write (`Apply`) forgets the values of that device; a read that was under
  way then is neither remembered nor joined by later readers, who get a read of their own once it has ended. The
  tab is drawn from `Last`, a busy line (`aria-busy`, the #538 pattern) fetches `GET /devices/{id}/settings/form`,
  and a failed refresh keeps the old values with the reason ("Refresh failed."). The background refresh of the data
  (`brokerHostReader`) keeps, in memory only, why its last try failed (#818): a class (`settings.FailReason`, never the
  error text) with the time of the failure and the time of the next try, which is the reader's own schedule. The
  Settings line says "Last background refresh failed 12m ago: the device did not answer. Next try 14:05." only while
  that failure is newer than the values shown; a refresh that works, or fails after the Settings step, ends it, as does
  a restart of TasmoFleet. GPIO templates: the service's
  existing reading cache is kept past the hour as a `stale` reading (shown, never applied, like a backup) unless a
  backup is newer; the tab's busy line asks `POST /policy/templates/read` (an htmx request answers the page body
  only; reads stay POST with the CSRF token) and says how many devices could not be read. A refresh that started
  is read to its end when the page that asked for it is left, and a read cut off by its caller marks no device as
  failed. Readings older than one hour (`StaleAfter`, a constant, not a setting) carry the visible marker "Stale".
  A refresh that arrives after the person has started editing the page is not swapped in (`data-guard-swap`); the
  line says so and keeps the age and the marker of the values shown.
- **Device data refresh** (#600): the data pages draw from stays current without a page open. The broker re-read
  of 6.4 (`brokerHostReader`) is the one scheduler; its periodic reads visit one online device per 30-second pass (a
  restart's refresh runs as soon as it is due, one device after the other), and a visit reads, in that order, what
  is due: the broker (every 6 h), the identity (every 24 h, `Prober.RefreshFacts`: the probe
  of an explicit refresh, MQTT first after the check that hears every reply on the topic, #682, but the stored name and
  topic do not follow the device's DeviceName and topic and no MAC is bound) and the data (every 24 h): the Settings values
  (`settings.Manager.Read`, which fills `Last`), the values the policy rules check (`policyReporter.readDevice`) and
  the GPIO template reading (`templates.Manager.Refresh`). A restart the health tracker records (INFO3, or the
  uptime drop seen in STATE) makes all three due once the device has settled (20 s); further announcements of
  the same restart move that time back and stay one refresh. A restart reads the identity and the data only if no
  restart read them in the last hour: the further restarts of a device in a boot loop read only the broker, and the
  identity and the data are read once when the hour is over (a failed read is not retried before, except the retry of a
  failed regular refresh, which keeps its 10-minute retry time once, #817), so about once an
  hour. The broker and the identity are read under the
  device lock (an operation a person starts on the device meanwhile, restore and Replace device included, waits up to
  5 s for it and for the reader's momentary lock checks instead of being refused as busy; what TasmoFleet starts on
  its own, a scheduled backup or the HTTP probe a device page asks for, is refused at once, #670); the data is read without it, after trying it (a device a job holds is skipped and visited again
  at the next pass), so a person's save is not refused as busy while a read runs; the settings save counter keeps a
  read that a save overtook out of `Last`. Identity fails closed: the broker read and the data read first check, with one `Status 5`, that the device at the stored address reports a MAC of the stored set (`Prober.VerifyAddress`, the rule of `ReadNet`; a device stored without a MAC has nothing to compare, minimal firmware reports none and is refused), and read nothing else from another device. A check vouches only for the transport its read used (#810): over HTTP it is answered by whoever holds the address, over MQTT by whoever listens on the topic, and a read the router lets fall back from one to the other may reach another device than a check routed on its own. Every caller of the prober therefore either asks the check first and pins its reads to the transport that answered it (`Prober.verifyAndPin`, the data read; `Prober.ReadNet`, whose `Status 5` is its check and whose `Status 11` follows it), or hands the check the trace of its reads (`Prober.VerifyRead`, the Settings tab's own read and the policy poller), which asks it pinned to each transport that answered them, over both after a read answered over both, and refuses the values when one of them does not show the stored MAC. A transport that carried a read but answered none (MQTT published it, nobody answered, and the read went on over HTTP) brought no value and is not asked; a trace in which nothing was answered is refused. Over MQTT these checks hear every reply on the topic (#804, #878: `Prober.hearTopic` through `Router.TopicStatus5`, 1.5 s after the first reply) for every device the router reads over MQTT or holds off its topic, since one reply there is whichever of two devices answered first: only exactly one reply with a stored MAC vouches, a second reply (a failed one included), another MAC or a reply without a MAC marks the device (below), and when that check learns nothing the check before the reads goes to the address alone (pinned to HTTP) and the check after the reads refuses. The data read asks its check through the router and reads the rest over the transport that check went over (#676): after an answer over MQTT every read of the refresh is pinned to MQTT (no fallback to HTTP, where another device may hold the address; a read MQTT leaves unanswered fails the refresh) and the checks after the reads are not asked again; otherwise everything goes over HTTP and the Settings and policy reads ask again after their reads (#662). A check answered over MQTT alone that finds another MAC on the topic marks the device in the router (#852, `Router.TopicTaken`): its reads go over HTTP while HTTP works and its writes and transfers never go to that topic, until a check that hears every reply on the topic (`Router.TopicStatus5`, 1.5 s; a window cut short hears nothing) hears its own MAC alone (the check of a marked device asks the topic this way first, and when it learns nothing nothing is read from the topic; a check that hears one reply only sets marks, never ends one), while its stored topic is another one (the mark is kept and counts again if the topic returns to it), or until TasmoFleet restarts; ReadNet, Refresh identity over MQTT and the restore's check before its upload (another MAC or a second reply) mark the device the same way, and Refresh identity over MQTT asks the topic of a marked device that way first, so a person can end the mark; the broker reader asks the topic of every marked device that way every 10 minutes on a schedule of its own, whatever HTTP's state and the gates of its reads (the mark sends the reads over HTTP, so the visits that check on their way may never come), until only the device answers there, and once, at most every 10 minutes and two per pass, the topic of an unmarked device that a discovery config named with another MAC (#876, T19; the claim itself marks nothing); the broker read, the data refresh and that topic check log it as a warning that names the topic, and a broker read or a data refresh that found it comes back after 10 minutes. Such a refusal is quiet (the identity refresh audits it) and is asked again after 6 h, also when a restart or a command asked for the read; a refused identity refresh holds the data read back for a day. Offline devices, devices the HTTP poll has not reached lately (but for the data alone when their reads go over MQTT and a MAC is stored, #676) and devices
  on minimal or safeboot firmware are skipped; a failed read is retried after 10 minutes, a refused identity refresh
  (another MAC answers at the address; audited) at the regular interval. Never `Status 0`; a visit sends its requests
  one after the other. The policy poller (system setting `PolicyLiveInterval`) and the MQTT updates keep feeding the
  policy values as before; while the live check is paused on the policy page or its interval is 0, the data read
  leaves the policy values out too. The refresh buttons that remain (Refresh identity, Check now,
  Read again) force a read and show the #538 busy state. There are no new settings and no table.
- SetOption numbers are always displayed with their internal name and a one-line
  description (e.g. "SetOption0 (`save_state`): restore relay state after power loss").

---

## 14. Fleet operations (M2)

### 14.1 Bulk commands

The device list's selection bar offers "Update", "Back up", "Send command", "Restart" (admins) and
power (operators; `POST /devices/bulk/power`). "Send command" is admin only: one command line,
a `Backlog` included, goes to every selected device. The command passes the same `cmdpolicy`
verdict, confirmation texts and redaction as the console, and the preview is the confirmation: a
token bound to the command line and the device list. The send runs as a `command` transaction (per-device
result, "Retry failed", audit). "Devices at a time" is 1, 3 (default) or 5. There is no configurable
allowlist: `bulk.DefaultAllowlist` is a leftover that nothing uses (design change; the policy of
the console decides).

### 14.2 Fleet settings templates

Planned as named command sets applied to tagged devices. Not built: the policy rules (§14.4) and
"Make compliant" cover the use case, and the policy editor offers rule templates ("Use template").

### 14.3 Restart

`Restart 1` as a bulk action ("Restart" in the selection bar, admin) with a confirmation page, run
as a `command` transaction with the same "Devices at a time" choice. A single device has its own
Restart (device page).

### 14.4 Policy and drift view

Declared rules (`policy.Rule`) with exceptions per device, tag or family. Values are read
from decoded backups (preferred) or live `Status` replies. The view lists drift per device.
Fixing it is an explicit bulk action. Example rules: PowerOnState 0,
SetOption0 0 (ESP32 exception: 1), SaveData 0, Timezone 99.

The device's Drift tab compares configuration, not readings: the live groups of the status snapshot (Status 7 time,
Status 8 and 10 sensors and energy, Status 11 runtime) and clock, uptime and counter leaves are left out of the
comparison and of "Show all" (#446). Restore verification has its own list and is unaffected.

A rule's command is free text with the curated settings as suggestions: any Tasmota command that
answers a plain query (`Sleep`, `LedState`, ...) can be a rule. It is read live by its bare name,
one command at a time, and set with `<Command> <want>`. Never allowed: `Status` in any spelling
(Status 0 is never sent automatically), secrets, commands that act when sent bare, and commands
whose set form needs a console confirmation, and a curated setting in another spelling: another letter case
(`hostname`) or an index the setting does not take (`Topic1`, `MqttHost0`: Tasmota drops the index and sets the
setting, #606), and a SetOption under another name: a SetOption synonym of the pinned Tasmota catalogues (`MqttTLS`
is `SetOption103`) or a SetOption index or value with a leading 0 (the index is a plain decimal on the device, so
`SetOption003` is `SetOption3`; the value is read as octal there, so `08` is 0). The
checks are `cmdpolicy.PolicyReadable`, `cmdpolicy.PolicySetText` and `policy.deviceName` (which reads the name
with `cmdpolicy.ResolveName`, as the router does); the claims are in
docs/security/command-policy.md (G1 to G6). A generic rule has no value validator and is not read from backups.
It is treated as not restarting the device: the deny list (G2) holds the commands known to restart, and one it
misses (a new or rarely used command) restarts the device in the middle of a run, which the read-back then reports.
`Sleep`, `SerialLog` and `SysLog` reply nested (`{"Sleep":{"50":{"Active":"100"}}}`); the rule
compares the configured value (the object key), not the active one.
`Latitude` and `Longitude` are stored by the device as float32 micro-degrees and read back with float32
noise, so they are compared at four decimals (11 m): `settings.CanonicalWant` reduces a rule's `want`
to that form in `policy.ParseRules` (a non-number or an out-of-range value is an invalid want), and
`policy.Rule.Want` carries the canonical form, which is what the check compares and Make compliant sends.
The stored `RuleSpec.Want` keeps what the admin typed. Every other command's `want` is used as typed.

**Rule ids.** Each rule has a stable `id` (`^r[a-z0-9]{1,15}$`, optional in the JSON format: `policy.Rule.ID`,
`sysconfig.RuleSpec.ID`) that survives edits, reordering and export/import, so a link names a rule and
not a slot. New rules get `r` plus 10 random base32 characters; ids are unique within a list (an import
with a duplicate or malformed id is refused). A list without ids (every version stored before 0.19, and
`POLICY_RULES` files without ids) gets `r1`...`rN` by position (`policy.LegacyID`) when it is read; stored
rows are never rewritten, the next save persists the ids. Opening and saving such a version unedited is
no change. Ids that were `r1`...`rN` at the upgrade keep that form for good and count as positions in the
version diff: it matches rules by id only for real (non-position) ids and by name otherwise, so a rename
of such a rule shows as removed plus added.

One evaluation: `policy.Outcomes` judges every enabled rule against every device in its scope and
returns one outcome per pair: Match, Deviate, Unknown, Shadowed (a higher rule on the same command
is the one evaluated; the outcome names it) or Excepted (the device is exempt; an excepted rule does
not claim the command). `policy.Check` is the Deviate and Unknown outcomes as findings, so Make
compliant and the page counts cannot disagree. `policy.Tally` and `policy.FleetTally` derive the
per-rule and fleet numbers: a device is Deviating with at least one Deviate, Not judged with an
Unknown and no Deviate, Compliant with no Required rule deviating or unknown. `PolicyService.Evaluate`
judges a draft rule list from cached values (no device call). `PolicyService.ReadValue` reads one
setting of one device to start a rule from it: a command refused by `policy.ReadProblem` or
`settings.Queryable` is never sent; a live read falls back to the newest decoded backup only for
commands a backup holds (`policy.BackupReadable`); a value with credentials or one that cannot be an
expected value is refused; the audit entry `policy.copy` names device and command, never the value.

**Overview (`/policy`).** Five tiles: Rules (enabled and disabled), Compliant devices, Deviating
(required and advisory), Not judged (all `policy.FleetTally`) and the live-check tile (state, last
run, Run now, Pause or Resume for admins, Retry now after a failure). Tabs follow (Rules, Deviating devices, History;
Import or export has its own page, `/policy/io`), then the rule table in stored order: a drag handle (admins), name (a link to
`/policy/rules/{id}`, Disabled chip), setting, applies to with the device count, Required or Advisory, a
compliance bar from `policy.Tally` read by rule id, and the row menu (Edit, Duplicate, Enable or Disable,
Move up, Move down, Delete). A disabled rule is dimmed and is not checked. A banner names the devices that
break a Required rule, with Show and Make compliant. Every list operation names its rule by id and the
version the person saw (stale: 409); the drag posts positions plus the version. Unknown query
parameters are ignored: the old `?edit=`, `?new=` and `?v=` no longer open an editor or a diff.
The empty page offers New rule and the first templates (admins), or says an admin can add rules.

**Run now and Check now (`POST /policy/live`).** The on-demand check is limited three ways: one run at a time,
at least one minute between runs (answered from the cache, with a notice), and a per-device cooldown of five
minutes (`web.LiveDeviceCooldown`). The cooldown counts from the oldest live read among the settings the device's rules
judge (scope, family, exception, shadowing and disabled rules decided by `policy.Outcomes`; a setting that cannot be read
live is not counted) and does not apply when one of them has no live value. So only a read that delivered every judged
setting holds the next check back; an MQTT stat of one setting, the Make compliant verify or a read that failed half way
move only their own setting and can neither hold a check back nor push it back (#989). A device no rule judges keeps
its newest read of any setting as the anchor. The result names every device the cooldown held back and when it can be read
again: a note on the three policy tabs (up to five names, then a count) and a banner on the rule page after Check now
(`?ok=cooldown&until=<unix>`, shown only for a time within the next cooldown, for devices the rule judges). Known cost:
a device that never returns a judged setting (State on an MQTT-only device, a command its firmware answers with Unknown) is read
by every Run now, at most once a minute, instead of every five minutes. No new device command and no new transport case.

**Rule editor** (`/policy/rules/new`, `/policy/rules/{id}`, admins; operators get the read-only rule page).
"Cancel" and "Save rule" both leave the editor for the overview: Cancel changes nothing, Save shows
the saved rule's name and marks its row (`?ok=saved&rule=<id>`). Duplicate (row menu) opens the editor with
an unsaved copy (`/policy/rules/duplicate`); nothing is stored until Save rule, and the copy lands right
below its source. "Copy from a device" (`/policy/rules/new/copy`) starts a draft from one device's value
the same way. Saving checks the version the editor was opened on (409 when someone else saved first).
The compliance column of the table has a fixed width of 230 px (bar and numbers).

The Deviating devices tab (`GET /policy/deviations`, `?show=required|advisory|unknown`, `?q=`) lists every
deviation grouped by device. Admins get a box per row read live (a backup-only value has none): Required rows
start ticked, Advisory rows never do. The boxes post `pick=<deviceID>:<ruleID>` to `/policy/fix`: with only
`pick` the plan is exactly the picked deviations. With `only`, `all` or `device` (device tile, overview banner) the
plan is every Required deviation of those devices, and an Advisory deviation joins only when its pick is
posted; the preview offers each as an unticked box ("Update plan"), the dialog a "Choose Advisory rows…"
button. A pick for a pair that is not a finding of the report, or of a device outside the selection, is dropped, and
the apply digest covers the picks.

The plan page (#1002) names the one selected device in its title ("Make kitchen compliant") and counts the
selected devices otherwise ("Make 3 devices compliant"), whether or not each gets commands. A device that deviates
from Advisory rules only has no commands yet; "No Required rule deviates. Tick an Advisory deviation below to
include it." is a hint on its card, not a block (a block is a reason the device cannot be changed: API off, a
running or planned job, nothing read live, already compliant), and with no command on the page the subtitle says
"Nothing to run yet: tick an Advisory deviation and choose Update plan" when a card carries that hint, and
"Nothing to run: no device on this page can be changed now; each card says why." otherwise. With JavaScript, Update plan is enabled only
while a tick differs from the drawn plan and Run only while none does; without it both stay enabled. Cancel goes
back to the opening device page when its tile posted the marker `from=device` and that device is the only one on the
plan, to the Deviating devices tab for a plan picked there, and to the policy page otherwise; the marker is a fixed
value the server compares and writes back (the preview's forms carry it, so it survives Update plan), never a
URL. Devices that cannot be judged sit in a fold: those with an unknown value a
backup decodes (`policy.BackupReadable`) get "Back up these N" (POST `/devices/bulk/backup`, admin), the others a
hint and Run now.

### 14.5 Notifications

Package `notify` tells a person about events they did not watch (#249). Events: `update.finished`
(one per update transaction, sent by the job callback when the last job of the transaction ends; result
`ok`, `partial`, `failed` or `cancelled`), `update.not_started` (a scheduled transaction none of whose jobs
started, 9.4b; result `not_started`, a problem: also under `problems`, priority as failed), `backup.scheduled_failed` (one per scheduled run, one line
per failed device with hostname, address and reason, linking to the failed rows of the Backups page), `snapshot.failed` (scheduled database snapshot), and `test`. Settings
(`NOTIFY_*`, Settings page, tab Notifications) select the events and configure two server-side channels, plus the browser (below):

- **Webhook:** POST to the address as typed, in one of four formats (`NOTIFY_WEBHOOK_FORMAT`, #469):
  `json` is the body `{event, result, title, text, url, time}`; `homeassistant` sends the same body
  (its webhook trigger exposes it as `trigger.json`); `ntfy` sends the text as a plain-text body
  with the `Title` header (RFC 2047 encoded when not ASCII, line breaks replaced by spaces),
  `Click` only when `PUBLIC_URL` gives a link, and `X-Priority` (not `Priority`, which a proxy may use for RFC 9218) 4 for failed, partial or not started results,
  3 otherwise; `gotify` sends `{title, message, priority}` as JSON (priority 8 for failed,
  partial or not started, 5 otherwise) with `extras["client::notification"].click.url` when there is a link
  (the token goes in the secret header `X-Gotify-Key`). An unknown format is refused, not sent as
  JSON. The settings page previews the saved format (request line, headers except the secret
  one, body). Optional secret header. Ten seconds per attempt, one retry on a network error, 5xx or 429, never on 4xx, no proxy
  from the environment, redirects only to the same host. The address is never part of a status or log text.
- **E-mail:** `net/smtp` with STARTTLS (never skipped when chosen), implicit TLS or none; PLAIN or
  LOGIN login; quoted-printable UTF-8 text with the title as subject.
- **Browser (#540):** a desktop notification while a TasmoFleet tab is open, for admins only (the opt-in
  sits on the admin Settings page). `notify.Options.Browser` is called once by `Dispatcher.Emit` for every event
  the settings select, whether or not a channel is on, never for `test`; the app publishes it as
  `events.TopicNotice` with an `events.Notice` payload (id, name, result, title, text, path; the id is
  `Event.ID`, set once by `Emit`: the start time plus a counter, equal in every stream and in the event's Web Push). The event stream sends it as the SSE event `notice` (one JSON
  line, path below the base path) and only on streams of admins; the role is re-checked at every session
  re-check like the other admin-only streams. `notify.js` shows it with the Notification API. The opt-in is per
  browser (localStorage; the permission is requested only after a click on "Enable browser notifications"),
  nothing is stored on the server. One notification per event across tabs: the tab holding a Web Lock shows it,
  the others forward what they heard over a BroadcastChannel, and the event id and the notification tag fold
  repeats. On pages without live data the leader tab opens `/events?only=notice` while notifications are on
  (an operator asking for it gets 403), so at most one extra stream per browser. The card says when the browser
  blocks notifications or the page is not a secure context (HTTPS or localhost). Nothing shows while no tab
  is open; Web Push (below) does.
- **Web Push (#610, server side; #541):** every event `Emit` hands to the tabs is also pushed to the browsers admins
  subscribed, through their vendors' push services (Google FCM, Mozilla autopush, Apple, Microsoft), so it shows
  with no tab open. VAPID (RFC 8292): one P-256 key pair made on the first start, the private key sealed with
  secretbox in `push_vapid` (never rotated; a key that cannot be opened because `secret.key` was replaced is
  replaced, which drops every subscription, with a warning). The token's audience is the endpoint's origin, it
  expires after 12 hours, and its `sub` is the fixed project address (never `PUBLIC_URL` or a mail address, which
  would tell the vendors an internal name). The payload is `{id, name, result, title, text, path}` (path below the
  base path), encrypted for the browser (RFC 8291, `aes128gcm`, one record, at most 4096 bytes: a long text is cut
  at a character and marked). Headers `TTL: 86400`, `Urgency: high` for failed, partial or not started results,
  else `normal`. Admin routes (CSRF as usual): `GET /settings/system/push/key` (the public key),
  `POST` and `DELETE /settings/system/push/subscription` (the browser's `PushSubscription.toJSON()`; an https
  endpoint without credentials, not a `localhost` name or a literal loopback, private, link-local, unspecified,
  multicast or 100.64.0.0/10 address or one with a zone, at most 2 KB, a P-256 `p256dh` and a 16-byte `auth`; host
  names are not resolved, so this is a check against mistakes, not an SSRF boundary), audited as `push.subscribe`
  and `push.unsubscribe` with the push service's host only. A subscription (`push_subscriptions`, one row per
  endpoint) belongs to the admin who made it; at every event the owner's role is
  read again (the resolver of job creators): not an admin any more (operator, local account gone or disabled)
  removes the row, a failure to read keeps it and skips that push. For single sign-on and header users the role is
  the one recorded at their last sign-in (header users: last request, at most every 10 minutes), so an SSO admin
  removed at the identity provider keeps receiving pushes until they sign in again; the Settings list (#611)
  lets any admin remove any subscription. 404 or 410 removes the row; a network error, 5xx or 429 is retried once; the outcome
  is kept on the row (`last_ok_at`, `last_error`). Same client as the webhook (ten
  seconds, no proxy from the environment, same-host redirects); a log line names the push service's host, never
  the endpoint, whose path is the browser's capability. Details: `docs/security/web-push.md`.
- **Web Push, in the browser (#611):** the Web Push card of Settings → Notifications (admins). Its first part is
  this browser's state, which only the browser knows (`push.js`): on a page that is not a secure context (plain
  HTTP) it says that HTTPS or localhost is needed instead of offering Subscribe; it also says when the browser
  has no Push API, when an iPhone or iPad has not added the app to the Home Screen, when notifications are
  blocked, and when this browser holds a subscription the server does not list or made for another key ("Not
  subscribed any more", with Subscribe again). Subscribe asks the permission in the click, registers the worker
  and subscribes with `userVisibleOnly`. The second part is the list of every subscribed browser (the server's, so
  it works without scripts and over plain HTTP): label (browser and system, made from the User-Agent of the
  subscribing request through a fixed table; `push_subscriptions.label`, migration 0050), owner, push service
  host, subscribed, last delivery or its `last_error`, with Send test (`POST /settings/system/push/test/{id}`:
  the same owner check and sender as an event, rate-limited like the channel tests, the result in the row) and
  Remove (`GET` confirmation page and `POST /settings/system/push/remove/{id}`, any admin, any row; removing
  this browser's row also drops the subscription the browser holds). A row is named by the SHA-256 of its
  endpoint (base64url), never the endpoint. Test and remove are audited (`push.test`, `push.remove`) with the
  push service's host only. The service worker `sw.js` is served at the base path (`/sw.js`, so its scope is
  the whole app), has no fetch handler, shows a push with `notify.js`'s mapping (the same tag as the open tab, so
  the browser folds the two into one) and opens only a page of its own origin on a click; it does not handle
  `pushsubscriptionchange` (no CSRF token): a changed subscription is found on the next visit to Settings.

Delivery runs in the background and never fails the caller. The last delivery and the last test per
channel are kept in memory (empty after a restart). The two secrets are write-only and stored with
the other secrets (secretbox) and bound to the target they were entered for (the webhook address in full;
mail host, port, security and user), like the MQTT password: a save that changes the target must enter the
secret again, and a test with another target in the form sends the stored secret nowhere. "Send test" posts the form of the tab, saves nothing and uses the
stored secret when the field is empty. Not in this version: device-offline events, HMAC signatures,
provider APIs, a delivery queue or history.

---

## 15. Authentication, authorisation and security

### 15.1 Modes

tasmofleet runs in three deployment shapes, all first-class and covered by e2e tests:
(a) behind a reverse proxy with an IdP (e.g. Traefik + Authentik), (b) behind a
forward-auth proxy, and (c) **standalone**: a plain Docker host on a LAN with no proxy
and no IdP. `AUTH_MODE` selects the primary method. Local accounts (`LOCAL_LOGIN`,
default on) can be combined with every mode and are the only method in `local` mode.

**OIDC (`AUTH_MODE=oidc`; the default mode is `local`)**

- Discovery from `OIDC_ISSUER` at start, retried with backoff. Readiness reports not ready
  until it succeeds.
- Authorization code flow with PKCE S256, `state` (32 random bytes) and `nonce`, kept in a
  short-lived (10 min) HttpOnly cookie bound to the login attempt.
- `redirect_uri = PUBLIC_URL + BASE_PATH + "/oidc/callback"`. It is never derived from
  request headers. `PUBLIC_URL` is required in this mode.
- The ID token is verified by go-oidc (issuer, audience, expiry, nonce, signature, JWKS
  rotation). Claims: `sub`, `OIDC_USERNAME_CLAIM` (`preferred_username`), `email`, `name`,
  `OIDC_GROUPS_CLAIM` (`groups`, a list of names in Authentik).
- Roles from groups (`ADMIN_GROUPS`, `OPERATOR_GROUPS`), highest wins. At
  least one of these lists must be configured; there is **no implicit "any authenticated
  user"** (config validation fails). No matching group: 403 with a clear page, no session.
- **Group revocation.** The refresh token is stored server-side with the session, sealed
  with secretbox. On a request at least `OIDC_RECHECK` (15 min) after the last check, the
  session refreshes at the IdP and re-evaluates the groups. A role change applies at once.
  Losing all mapped groups, or a failed refresh, ends the session. Defaults: idle timeout
  `SESSION_IDLE` 2 h, absolute `SESSION_MAX` 12 h. The role is also recomputed on every
  request from the stored groups and the current mapping, so a config change applies
  immediately.
- Logout clears the session, then does an RP-initiated logout: redirect to
  `end_session_endpoint` with `id_token_hint` and `post_logout_redirect_uri`
  (`OIDC_POST_LOGOUT_URL`, default `PUBLIC_URL + BASE_PATH + "/"`).
- Post-login redirect targets are local paths only (must start with `/`, not `//` or `/\`,
  no scheme).

**Trusted header (`AUTH_MODE=header`)**, e.g. Authentik outpost forward auth in Traefik:

- Headers `HEADER_USER` (`X-authentik-username`), `HEADER_GROUPS` (`X-authentik-groups`,
  separator `|`), `HEADER_EMAIL`, `HEADER_NAME`. The same group-to-role mapping applies and
  is equally mandatory.
- Accepted only if `RemoteAddr` is in `TRUSTED_PROXIES`. `X-Forwarded-For` is never used
  for this decision. From an untrusted peer the headers are ignored.
- No session; every request is authenticated by its headers. Logout redirects to
  `HEADER_LOGOUT_URL`. The CSRF token is an HMAC of the user, keyed with a persisted key
  (`<DATA_DIR>/csrf.key`, 0600, 32 random bytes), so forms survive restarts.
- Sessions created in another mode are rejected after a mode switch.

**Local accounts (`AUTH_MODE=local`, or alongside oidc/header with `LOCAL_LOGIN=true`)**

- Multiple users, each with role `operator` or `admin`, and a
  `disabled` flag. Login on the one sign-in page `/login` (form POST to `/login/local`).
- **One login page** (decision 2026-10-19). `/login` shows, side by side, a button
  "Sign in with `<OIDC_DISPLAY_NAME>`" (when OIDC is configured; the name is a sign-in
  setting, default "Single sign-on", works for any provider) and the username/password
  form (when local login is on and a local account exists). The browser is redirected to the
  IdP automatically only when OIDC is the only method. The button is `GET /login/sso`;
  there is no `GET /login/local` (the form only posts there). Header mode is unchanged (the page appears
  only when the proxy did not identify the visitor and a local account exists).
- **Break-glass** is a local admin on this page plus `tasmofleet user passwd` on the
  host (runbook `docs/runbooks/emergency-access.md`). The direct admin listener
  (`ADMIN_LISTEN`, 2026-10-18) was removed on 2026-10-19; there is no protection beyond
  the login rate limiting.
- Management:
  - CLI: `tasmofleet user add|passwd|role|disable|enable|delete|list` (password from stdin
    or a TTY prompt, never as an argument).
  - Web UI (`/users`), **admins only**. The list of all principals (local accounts plus the
    SSO and proxy users seen at sign-in: name, method, role, last login, live sessions)
    needs no step-up. Every change needs a step-up valid for 5 min: local admins re-enter
    the password, OIDC admins re-authenticate at the IdP (`POST /oidc/stepup`). Changes:
    create users, set roles, disable, reset passwords, delete; for SSO users only "End
    sessions" (their roles come from the IdP groups). An operator can never create users
    or raise roles. Users change their own password only with the current one. Any
    password change or disable revokes that user's sessions.
  - Header principals have no session and nothing to confirm with: the page is view only
    for them.
- First run: if no local user exists and local login is on, an `admin` account is created when
  `AUTH_MODE=local` or `INITIAL_ADMIN_PASSWORD` is set (an OIDC install is not seeded with a surprise
  account). Without `INITIAL_ADMIN_PASSWORD` a random password is generated and written to
  `<DATA_DIR>/initial-admin-password` (0600, created exclusively). It is **never logged**; the log
  names the file and prints a one-time **setup link** (`/setup/account`, valid until the account has a
  password of its own) with which the admin sets the password. Signing in with the file's password
  works too. The account has `must_change_password`; the first login forces a change before any other
  page. The file is deleted after the change, and at the next start. The first-run wizard (`/setup`)
  follows: device networks, optional MQTT, optional SSO, first scan.
- Account invariants, attackers and tests: `docs/security/account-lifecycle.md`
  (password generations revoke sessions and device cookies atomically; actor-checked
  mutations; last-admin guard).
- Password hashing: PBKDF2-HMAC-SHA256, 600 000 iterations, 16-byte salt, PHC-like string
  `$pbkdf2-sha256$i=600000$<salt>$<hash>`, constant-time compare. Decision: PBKDF2 over
  argon2id because it is in the standard library (`crypto/pbkdf2`, FIPS-approved),
  already implemented and tested, and meets the OWASP 2023 parameters. argon2id would add
  `golang.org/x/crypto` for a marginal gain at this scale. At most 2 hash checks run
  concurrently (CPU bound); a waiting client that disconnects gives up its slot.
- Rate limiting, without account lockout. **Authoritative:**
  `docs/security/login-rate-limiting.md` (attacker models, exact numbers, claims with
  tests, accepted residual risks). Summary: the network is an IPv4 address or IPv6 /64,
  taken from `httpx.ClientIP` (X-Forwarded-For only from trusted proxies). There are
  budgets per (network, user) with exponential delay capped at 15 min, per network across
  users, and per user across networks. Unknown users go through the same limiter and a
  dummy hash, and every attempt is counted at admission. _Recognised clients_ skip the
  shared network and user tiers and have their own small per-user tier: clients with a
  valid **device cookie** (HMAC-bound to the username, key derived from the persisted
  secret, max age 180 days, invalid after a password change). Memory is bounded: hashed keys, a fixed
  maximum per table, and an overflow bucket per tier instead of eviction, so a flood
  cannot flush an attacker's own counters. Accepted residual risk: a saturating botnet
  can slow a login from a new device (R1). Mitigations are the
  device cookie, the host CLI, and OIDC as the primary login. Every
  attempt is audited. Per-cookie revocation (a `device_cookies` table) is WP6b.
- Keys for CSRF tokens, device cookies and session signing are derived per purpose from
  `<DATA_DIR>/csrf.key` (32 random bytes, 0600, created on first start). They are separate
  from `secret.key`, which only encrypts data at rest, and they survive restarts.

### 15.2 Sessions

Server-side (`sessions` table). The cookie holds 32 random bytes; the store holds their
SHA-256. Sessions carry the sealed OIDC refresh token and the time of the last recheck
(OIDC only). Flags: `HttpOnly`, `SameSite=Lax` (needed for the top-level OIDC callback),
`Secure` when the request is HTTPS. The name is `__Host-tasmofleet` when Secure and the
path is `/`, otherwise `tasmofleet`. `Path` = base path, or `/` for `__Host-`. The ID is
rotated at login. Idle and absolute expiry apply, and expired sessions are cleaned hourly.
Every request re-validates the session: local user still exists, is enabled, password
unchanged since login, and `LOCAL_LOGIN` still on; OIDC session in OIDC mode only. Health
endpoints never create sessions (TasmoAdmin lesson A4). CLI `tasmofleet sessions revoke
[--user <name>]` ends sessions.

### 15.3 Roles

Two roles. Every authenticated principal has one of them; no mapped role means no access.

| Role     | May                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| operator | read everything except backup downloads and free-text backup values (diffs show the redacted values; free-text settings show only that they changed); OTA batches, backups, power toggle (single and bulk), read and harmless console commands (`cmdpolicy` classes read and operator), cancel jobs (not GPIO template transactions or kinds this build does not know: admin only), refresh a device's identity, change own local password |
| admin    | + inventory edits, discovery, import, firmware management, any other console or bulk command (`cmdpolicy` class admin), restore, backup download and delete, device settings, system settings, policy rule editing, audit log, About, local user management (with step-up)                                                                                                                                                                 |

Example: `Tasmofleet Users` -> operator, `Tasmofleet Admins` -> admin.

### 15.3a TLS and standalone operation

- `TLS_CERT_FILE` + `TLS_KEY_FILE`: the UI listener serves HTTPS. Files are re-read on
  SIGHUP.
- Neither certificate files nor a proxy nor the opt-in below (the zero-configuration mode; `TLS_SELF_SIGNED` was removed in 0.17):
  an ECDSA P-256 certificate is generated once and persisted in
  `<DATA_DIR>/tls/` (0600 key). It is valid 1 year and renewed 30 days before expiry. The
  startup warns that browsers will show a warning.
- Plain HTTP is allowed only behind `TRUSTED_PROXIES` (TLS ends at the proxy) or with an
  explicit `ALLOW_INSECURE_HTTP=true` (trusted LAN). Otherwise the process refuses to start
  and names the options.
- `PUBLIC_URL` is optional outside OIDC mode. Without it, absolute URLs for display only
  (e.g. on the About page) come from the request, and only when no trusted proxy is
  configured. They are never used for redirects that matter for security.
- The firmware listener stays plain HTTP in every mode (ESP8266 constraint).

### 15.4 Reverse proxy and external URL handling (package `httpx`)

- `PUBLIC_URL` (e.g. `https://tasmofleet.example.com`) is the only source for absolute
  URLs (OIDC redirect, logout redirect, links in notifications).
- `X-Forwarded-Proto`, `X-Forwarded-Host`, `X-Forwarded-Prefix` and `X-Ingress-Path` are
  honoured only from `TRUSTED_PROXIES`. They affect the base path for links and the
  scheme seen by logging, never authentication or redirect targets.
- Base path: `BASE_PATH` (static), or per request `X-Forwarded-Prefix` / `X-Ingress-Path`
  from a trusted proxy. Templates build every URL from it. HTMX requests use the same
  prefix. Static files and SSE live under it.

### 15.5 CSRF and browser hardening

- `http.CrossOriginProtection` (Sec-Fetch-Site / Origin) on every non-safe method, with
  `PUBLIC_URL`'s origin added as trusted origin, plus a per-session synchronizer token
  (form field `csrf` or header `X-CSRF-Token`, which HTMX sends via `hx-headers`).
  Login POST (`/login/local`) uses a pre-session token cookie (double submit).
- CSP: `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:;
connect-src 'self'; frame-ancestors 'self'; form-action 'self' <issuer origin>`. `frame-ancestors` is not configurable (the code passes no extra sources).
  No inline scripts (HTMX config via meta/attributes). `X-Content-Type-Options: nosniff`,
  `Referrer-Policy: same-origin`.
- All output goes through templ, which escapes every value. Device-provided strings are untrusted.

### 15.6 Threat model

Assets: control over devices (power, firmware), device credentials, Wi-Fi/MQTT secrets
in backups, the OIDC client secret, firmware integrity, the audit trail.

| Threat                                                                | Mitigation                                                                                                                                                                                                                                                                                                            |
| --------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Wrong-family or corrupted firmware bricks a device                    | Guard (§7.5), header inspection, dual-source SHA-256, size limits, mutation-tested                                                                                                                                                                                                                                    |
| Interrupted update leaves the device on minimal (TasmoAdmin A1)       | server-side persisted jobs, resume, probe-before-retry                                                                                                                                                                                                                                                                |
| Duplicate `Upgrade` from blind retries (A1)                           | write-ahead state, at most 2 attempts per stage, probe first                                                                                                                                                                                                                                                          |
| SSRF via device address, scan range, firmware URL, MQTT host (B4, A6) | Targets only from inventory, dial-time CIDR check, scans only inside `DEVICE_NETWORKS`, firmware hosts allowlisted per source, broker only from config                                                                                                                                                                |
| Forged identity headers                                               | trusted only from `TRUSTED_PROXIES` by RemoteAddr                                                                                                                                                                                                                                                                     |
| CSRF / cross-site actions                                             | CrossOriginProtection + token + SameSite                                                                                                                                                                                                                                                                              |
| XSS via device-supplied names/replies (A6, B3)                        | templ escaping, CSP without inline script                                                                                                                                                                                                                                                                             |
| Credential takeover through the web session (A6, B5)                  | no passkeys; local user management only for admins with step-up (current password), own password change needs the current one, password change/disable revokes sessions, no open registration (first admin from the environment or a generated password in a 0600 file, with a one-time setup link and forced change) |
| Session theft / fixation (A4)                                         | server-side sessions, rotation, HttpOnly/Secure/`__Host-`, no files                                                                                                                                                                                                                                                   |
| Brute force / lockout abuse on local login (B5)                       | three-tier rate limit without account lockout, max 2 concurrent hash checks, audit, min. 12 characters, can be disabled                                                                                                                                                                                               |
| Open redirect after login                                             | local-path check                                                                                                                                                                                                                                                                                                      |
| Device password leakage to the browser or logs (B2)                   | never rendered; Basic auth header; redaction; sealed at rest with key in `secret.key`                                                                                                                                                                                                                                 |
| Backup secret leakage (§12.3)                                         | 0600, admin-only download, redacted views, audited                                                                                                                                                                                                                                                                    |
| Mass assignment / Backlog injection in settings (B3)                  | typed fields, per-field commands, `;` rejected                                                                                                                                                                                                                                                                        |
| Restore blob behind auth (B8)                                         | pushed with the device's own restore upload (`/rs` + `/u2`, Basic auth and Referer as for `/dl`), to the one IP checked by MAC; never served by TasmoFleet (#430)                                                                                                                                                     |
| Malicious upstream release                                            | cannot be ruled out; dual-source compare, cache by SHA-256, releases only on explicit refresh, never auto-update devices                                                                                                                                                                                              |
| IoT VLAN client abusing the firmware port                             | read-only, only indexed files, no secrets, rate-limited per IP                                                                                                                                                                                                                                                        |
| Loss of the database                                                  | WAL, pre-migration copies, everything in `/data` for host backups                                                                                                                                                                                                                                                     |
| Lost updates from concurrent writers (B1)                             | SQLite transactions, unique indexes, optimistic state check in `SaveTransition`                                                                                                                                                                                                                                       |

---

## 16. Configuration

**Principle: a fresh install needs no configuration.** `docker run -p 8443:8080 -v data:/data tasmofleet`
(`8443` is the host port you map, the UI inside the container is always on `8080`; pick any free host port)
serves a self-signed certificate (no proxy, no certificate and no `ALLOW_INSECURE_HTTP`
set), creates the first local admin (random password in `<DATA_DIR>/initial-admin-password`, a one-time setup link in the log,
changed at the first sign-in) and shows the setup (device networks, optional MQTT, optional
single sign-on, first scan). Everything else is adjusted in the web UI.

Two tiers:

1. **Bootstrap and security settings: environment variables `TASMOFLEET_<KEY>`** or the same keys
   (prefix optional) in `/data/tasmofleet.env` (`KEY=value`, `#` comments). The environment
   wins. Secret keys also accept `<KEY>_FILE`. They are needed before the database exists or must
   never be changeable from the web; the System settings page lists them read-only (secrets
   masked). The authoritative list is the `env` tags of `internal/config.Config` without a `ui` tag; the operator-facing reference is `docs/configuration.md` (checked against the tags by `configdoc_test.go`).
2. **Operational settings: the System settings page** (`/settings/system`, admin), stored in SQLite
   (`internal/sysconfig`, `docs/security/system-settings.md`). They are no longer environment
   variables. Sign-in settings (`ui:"lock"`) are the exception: stored in the database unless
   the environment sets them, then the environment wins and the field is read-only.

| Key (bootstrap, environment only)                                                                             | Default                                                    | Meaning                                                                                |
| ------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| `DATA_DIR`                                                                                                    | `/data`                                                    |                                                                                        |
| `CONFIG_FILE`                                                                                                 | `<DATA_DIR>/tasmofleet.env`                                | the env file to read                                                                   |
| `LISTEN`                                                                                                      | `:8080` (one port in every mode; containers do not set it) | UI/API listener                                                                        |
| `FIRMWARE_LISTEN`                                                                                             | `:80`                                                      | device-facing firmware listener                                                        |
| `PUBLIC_URL`                                                                                                  | —                                                          | external URL; required for oidc, optional otherwise                                    |
| `BASE_PATH`                                                                                                   | `""`                                                       | static prefix, e.g. `/tasmofleet`                                                      |
| `TRUSTED_PROXIES`                                                                                             | —                                                          | CIDRs; required for header mode; with a proxy the listener stays plain HTTP            |
| `HEADER_USER`, `HEADER_NAME`, `HEADER_EMAIL`, `HEADER_GROUPS`, `HEADER_GROUPS_SEPARATOR`, `HEADER_LOGOUT_URL` | Authentik names, `\|`                                      | header mode (`AUTH_MODE=header` is environment only)                                   |
| `OIDC_POST_LOGOUT_URL`                                                                                        | `PUBLIC_URL`+base                                          |                                                                                        |
| `LOCAL_LOGIN`                                                                                                 | `true`                                                     | local accounts on the login page (always on in `local` mode); break-glass              |
| `SESSION_IDLE`, `SESSION_MAX`, `OIDC_RECHECK`                                                                 | `2h`, `12h`, `15m`                                         |                                                                                        |
| `INITIAL_ADMIN_PASSWORD(_FILE)`                                                                               | random, logged once                                        | first local admin, must change at first login                                          |
| `TLS_CERT_FILE`, `TLS_KEY_FILE`, `ALLOW_INSECURE_HTTP`                                                        | —, —, `false`                                              | §15.3a; with none of them and no `TRUSTED_PROXIES` a self-signed certificate is served |
| `TLS_SANS`                                                                                                    | —                                                          | extra DNS names/IPs for the self-signed certificate                                    |
| `DECODER_PATH`                                                                                                | `/usr/local/bin/decode-config`                             | empty disables backup decoding                                                         |
| `FIRMWARE_UPSTREAM_OVERRIDE`                                                                                  | —                                                          | tests and development only                                                             |
| `LOG_FORMAT`                                                                                                  | `json`                                                     |                                                                                        |

| Setting (System settings page)                                                                                                                                                                                             | Default                                                                                      | Applies                                                                                        |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `AUTH_MODE` (`local`, `oidc`), `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_SCOPES`, `OIDC_GROUPS_CLAIM`, `OIDC_USERNAME_CLAIM`, `OIDC_NAME_CLAIM`, `OIDC_DISPLAY_NAME`, `ADMIN_GROUPS`, `OPERATOR_GROUPS` | `local`, …, `openid,profile,email`, `groups`, `preferred_username`, `name`, `Single sign-on` | restart; step-up for all but the button name and scopes (2026-10-20); environment locks        |
| `DEVICE_NETWORKS`                                                                                                                                                                                                          | — (no device is contacted until set)                                                         | live                                                                                           |
| `FIRMWARE_BASE_URL`, `FIRMWARE_SOURCES`, `FIRMWARE_ALLOW_SINGLE_SOURCE`, `FIRMWARE_REFRESH_INTERVAL`                                                                                                                       | —, `github,ota.tasmota.com`, `false`, `6h`                                                   | restart (the firmware cache, the device-facing listener and the OTA routes are built at start) |
| `OTA_PARALLEL`, `OTA_RESTORE_OTAURL`                                                                                                                                                                                       | `2`, `true`                                                                                  | live                                                                                           |
| `BACKUP_KEEP`, `BACKUP_MAX_AGE`, `BACKUP_DAYS`, `BACKUP_TIME`                                                                                                                                                              | `10`, `720h`, none (manual), `03:00`                                                         | live                                                                                           |
| `POLL_INTERVAL`, `TELEPERIOD`                                                                                                                                                                                              | `5m`, `300s`                                                                                 | live                                                                                           |
| `MQTT_URL`, `MQTT_USER`, `MQTT_PASSWORD`, `MQTT_ACL_WARNING_HIDDEN`, `MQTT_FULLTOPIC`                                                                                                                                                             | —, —, —, —, `%prefix%/%topic%/`                                                                 | live (the broker connection is switched in place)                                              |
| `MQTT_PASSWORD_FROM_BACKUPS` (take each device's MqttPassword from its newest decoded backup, #609)                                                                                                                                          | `true`                                                                                          | live (the next backup, and a pass over the existing backups when turned on)                    |
| `DEVICE_DEFAULT_PASSWORD` (secret; prefill for add device / accept proposal, user `admin`)                                                                                                                                 | —                                                                                            | live                                                                                           |
| `LOG_LEVEL` (`debug`, `info`, `warn`, `error`; the environment locks it)                                                                                                                                                   | `info`                                                                                       | live                                                                                           |
| `DISCOVERY_MQTT`, `DISCOVERY_SCAN`, `DISCOVERY_SCAN_INTERVAL`, `DISCOVERY_SCAN_CIDRS`                                                                                                                                      | `true`, `false`, `24h`, —                                                                    | live                                                                                           |
| `METRICS_ENABLED`, `METRICS_DEVICE_LABELS`, `AUDIT_RETENTION`                                                                                                                                                              | `true` (off: `/metrics` is 404), `false`, `8760h`                                            | live                                                                                           |
| `METRICS_TOKEN(_FILE)` (secret, write-only; the environment locks it)                                                                                                                                                      | empty = only local scrapes                                                                   | live                                                                                           |
| `POLICY_LIVE_INTERVAL` (policy check background reads)                                                                                                                                                                     | `1h` (0 = off)                                                                               | live                                                                                           |
| policy rules (editor, versioned)                                                                                                                                                                                           | none                                                                                         | live                                                                                           |

**Upgrade from environment-based setups.** A removed operational variable that is still set
(environment or env file) is imported once on the first start of the new version: only into keys
the database does not hold yet, with a warning per key ("imported … from the environment; remove it
from the container configuration"); credentials in a legacy `MQTT_URL` move to the sealed
password; `POLICY_RULES` (inline JSON or a file) becomes the first managed rule version. After that
the variables are ignored (warning). `tasmofleet settings export|import <file>` moves the
settings and the policy rules as JSON for automated setups (secrets are never exported).

**Policy history.** Every save of the policy rules is a stored version (append-only). The history
view is derived: consecutive versions of one actor with each gap at most 2 minutes fold into one
entry (a revert, an import and the current version never fold; nothing is stored), days are grouped
in the server's zone, and each entry carries a summary from the diff against the version before it
(rules matched by id, or by name for versions stored without ids). `sysconfig.Rules.History` pages
it with a `before` cursor; `sysconfig.RuleHistory` lists the versions that touched one rule.

Validation errors at start name the key and stop the process. Settings saved on the page use the
same validators; a stored value that stops validating is skipped with a log line and shown on the page.

**Home Assistant profile (#442, part of #209).** When `SUPERVISOR_TOKEN` is set (a Home Assistant app),
`config.Load` reads `/data/options.json` (fixed, not `DATA_DIR`), which the Supervisor writes, as one more bootstrap source
and applies the profile defaults (`LISTEN=:8080`, `FIRMWARE_LISTEN=:80`,
`LOG_FORMAT=text`). Precedence: environment, env file, option, profile default. The file must exist and
contain only known options (`initial_admin_password` so far); anything else stops the start with an error
naming the file. Options are bootstrap and security settings only, so the principle above is unchanged:
operational settings stay on the settings page. Without the token nothing of this runs, so Docker and
Unraid installs are unaffected. The options are listed in `docs/configuration.md` ("Home Assistant app")
and must equal the `schema` of `deploy/homeassistant/tasmofleet/config.yaml` (tests).

---

## 17. HTTP routes

UI listener (all paths relative to the base path; `R` = role needed; non-GET = CSRF checked). The
authoritative lists are `internal/web/web.go` (and the `register*` functions it calls),
`internal/auth/service.go`, `internal/auth/accountweb.go` and `internal/app/app.go`. Pages that exist
only when their feature is wired (updates, firmware, policy, ...) are listed with it.

| Method    | Path                                                                                                                                                                                                                                                                                                     | R                                 | Purpose                                                                                                                                                                                                                                                                                                              |
| --------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --- | ------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| GET       | `/healthz`                                                                                                                                                                                                                                                                                               | none                              | liveness, no session, no DB write                                                                                                                                                                                                                                                                                    |
| GET       | `/readyz`                                                                                                                                                                                                                                                                                                | none                              | DB ping, OIDC discovery done, firmware listener up                                                                                                                                                                                                                                                                   |
| GET       | `/metrics`                                                                                                                                                                                                                                                                                               | token or local scraper            | Prometheus; 404 when `METRICS_ENABLED` is off                                                                                                                                                                                                                                                                        |
| GET       | `/static/*`, `/favicon.ico`                                                                                                                                                                                                                                                                              | none                              | embedded assets                                                                                                                                                                                                                                                                                                      |
| GET       | `/login`                                                                                                                                                                                                                                                                                                 | none                              | sign-in page (IdP button and local form); redirects to the IdP if OIDC is the only method                                                                                                                                                                                                                            |
| GET       | `/login/sso`                                                                                                                                                                                                                                                                                             | none                              | start the OIDC flow (the button)                                                                                                                                                                                                                                                                                     |
| GET       | `/oidc/callback`                                                                                                                                                                                                                                                                                         | none                              | OIDC redirect target                                                                                                                                                                                                                                                                                                 |
| POST      | `/login/local`                                                                                                                                                                                                                                                                                           | none                              | checks the local login (there is no GET)                                                                                                                                                                                                                                                                             |
| POST      | `/logout`                                                                                                                                                                                                                                                                                                | any                               | logout (+ RP-initiated)                                                                                                                                                                                                                                                                                              |
| POST      | `/oidc/stepup`                                                                                                                                                                                                                                                                                           | admin                             | step-up by re-authentication at the IdP                                                                                                                                                                                                                                                                              |
| GET       | `/`                                                                                                                                                                                                                                                                                                      | operator                          | dashboard: six stat tiles, fleet health bar, "Needs attention", transactions                                                                                                                                                                                                                                         |
| GET       | `/devices`, `/devices/table`                                                                                                                                                                                                                                                                             | operator                          | device list (filters `q`, `health`, `family`, `tag`, `chip`; rows live via SSE)                                                                                                                                                                                                                                      |
| GET, POST | `/devices/new`                                                                                                                                                                                                                                                                                           | admin                             | add a device (with probe)                                                                                                                                                                                                                                                                                            |
| GET       | `/devices/{id}`, `/devices/{id}/history`, `/devices/{id}/live`                                                                                                                                                                                                                                           | operator                          | overview tab, health history, live values                                                                                                                                                                                                                                                                            |
| GET, POST | `/devices/{id}/edit`, `/devices/{id}`                                                                                                                                                                                                                                                                    | admin                             | edit                                                                                                                                                                                                                                                                                                                 |
| GET, POST | `/devices/{id}/delete`, `/devices/{id}/restart`                                                                                                                                                                                                                                                          | admin                             | confirm and delete / restart one device                                                                                                                                                                                                                                                                              |
| POST      | `/devices/{id}/probe`                                                                                                                                                                                                                                                                                    | operator                          | refresh identity                                                                                                                                                                                                                                                                                                     |
| POST      | `/devices/{id}/name/adopt`, `/devices/{id}/name/keep`                                                                                                                                                                                                                                                    | admin                             | answer the "Name on the device" notice (§5)                                                                                                                                                                                                                                                                          |
| POST      | `/devices/{id}/fwreach/forget`                                                                                                                                                                                                                                                                           | admin                             | forget what is known about the device reaching the firmware server (back to unknown; audit `device.fwreach`)                                                                                                                                                                                                         |
| GET, POST | `/devices/{id}/replace`, `/devices/{id}/replace/preview`                                                                                                                                                                                                                                                 | admin                             | Replace device (new hardware, same identity)                                                                                                                                                                                                                                                                         |
| GET, POST | `/devices/{id}/settings`                                                                                                                                                                                                                                                                                 | admin                             | settings editor                                                                                                                                                                                                                                                                                                      |
| GET, POST | `/devices/{id}/console`                                                                                                                                                                                                                                                                                  | operator                          | console; `cmdpolicy` raises the needed role to admin per command                                                                                                                                                                                                                                                     |
| POST      | `/devices/{id}/power/{n}`                                                                                                                                                                                                                                                                                | operator                          | power toggle                                                                                                                                                                                                                                                                                                         |
| POST      | `/devices/bulk/power`                                                                                                                                                                                                                                                                                    | operator                          | power for the selection                                                                                                                                                                                                                                                                                              |
| POST      | `/devices/bulk/command`, `/devices/bulk/command/run`, `/devices/bulk/restart`, `/devices/bulk/restart/run`, `/devices/bulk/backup`                                                                                                                                                                       | admin                             | bulk Send command, Restart, Back up (§14.1)                                                                                                                                                                                                                                                                          |
| GET       | `/devices/bulk/result/{batch}`                                                                                                                                                                                                                                                                           | admin                             | result of a bulk run; a backup run lists its devices in the order they started, name A to Z (case-insensitive, then device ID), whatever order they were posted in, with the device ID as a muted `#N` after the name (#993); the other kinds keep the posted order                                                                                                                                                                                                                                                                                                 |
| POST      | `/devices/{id}/backup`                                                                                                                                                                                                                                                                                   | operator                          | backup now (1 minute cooldown per device, §12.4)                                                                                                                                                                                                                                                                     |
| GET       | `/devices/{id}/backups`, `/devices/{id}/backups/diff`, `/devices/{id}/drift`                                                                                                                                                                                                                             | operator                          | Backups tab, diff and Drift tab (free text hidden for operators)                                                                                                                                                                                                                                                     |
| POST      | `/backups/{id}/keep`                                                                                                                                                                                                                                                                                     | operator                          | pin or unpin a backup run                                                                                                                                                                                                                                                                                            |
| GET       | `/backups/{id}`                                                                                                                                                                                                                                                                                          | admin                             | download (audited)                                                                                                                                                                                                                                                                                                   |
| GET, POST | `/backups/{id}/delete`, `/devices/{id}/backups/delete`; GET `/devices/{id}/backups/prune`                                                                                                                                                                                                                | admin                             | delete backups                                                                                                                                                                                                                                                                                                       |
| GET, POST | `/backups/{id}/restore`                                                                                                                                                                                                                                                                                  | admin                             | restore (confirm, start)                                                                                                                                                                                                                                                                                             |
| GET       | `/restores/{id}`, `/restores/{id}/status`                                                                                                                                                                                                                                                                | admin                             | restore progress                                                                                                                                                                                                                                                                                                     |
| GET       | `/firmware`, `/firmware/releases/{version}`                                                                                                                                                                                                                                                              | operator                          | releases, cache, release notes                                                                                                                                                                                                                                                                                       |
| POST      | `/firmware/refresh`, `/firmware/fetch`                                                                                                                                                                                                                                                                   | admin                             | index refresh / fetch one file                                                                                                                                                                                                                                                                                       |
| GET, POST | `/firmware/artifacts/{id}/delete`; POST `/firmware/delete-selected`                                                                                                                                                                                                                                      | admin                             | confirm / delete cached files (§8.2a)                                                                                                                                                                                                                                                                                |
| POST, GET | `/firmware/prefetch`, `/firmware/prefetch/{id}`, `/firmware/prefetch/{id}/stream`                                                                                                                                                                                                                        | admin                             | prefetch the fleet's files of a release, progress (§8.2a)                                                                                                                                                                                                                                                            |
| POST      | `/firmware/custom`                                                                                                                                                                                                                                                                                       | admin                             | upload custom firmware (§8.6)                                                                                                                                                                                                                                                                                        |
| GET, POST | `/updates/new`                                                                                                                                                                                                                                                                                           | operator                          | select devices, release, policy                                                                                                                                                                                                                                                                                      |
| POST      | `/updates/preview`                                                                                                                                                                                                                                                                                       | operator                          | dry run table                                                                                                                                                                                                                                                                                                        |
| POST      | `/updates`                                                                                                                                                                                                                                                                                               | operator                          | submit batch                                                                                                                                                                                                                                                                                                         |
| GET       | `/transactions`                                                                                                                                                                                                                                                                                          | operator                          | transaction history (updates, policy fixes, commands, backups)                                                                                                                                                                                                                                                       |
| GET       | `/transactions/{batch}`, `/transactions/{batch}/live`                                                                                                                                                                                                                                                    | operator                          | batch progress (SSE)                                                                                                                                                                                                                                                                                                 |
| GET, POST | `/transactions/{batch}/cancel`, `/transactions/{batch}/retry`, `/jobs/{id}/cancel`                                                                                                                                                                                                                       | operator                          | cancel a batch or job; retry the failed devices. Admin only (403 for operators, decided before the job kind is looked at) for GPIO template transactions and any job kind the build does not know; a template transaction is never retried (409, the page offers "Check again…", which opens the GPIO templates tab) |
| GET       | `/policy`, `/policy/deviations`, `/policy/history`, `/policy/io`, `/policy/io/export`                                                                                                                                                                                                                    | operator                          | overview, deviating devices, version history, import or export page, export                                                                                                                                                                                                                                          |
| GET       | `/policy/rules/{id}`                                                                                                                                                                                                                                                                                     | operator                          | rule page (read-only for operators)                                                                                                                                                                                                                                                                                  |
| POST      | `/policy/live`                                                                                                                                                                                                                                                                                           | operator                          | run the live check now                                                                                                                                                                                                                                                                                               |
| POST      | `/policy/live/pause`, `/policy/live/resume`                                                                                                                                                                                                                                                              | admin                             | pause or resume the background reads                                                                                                                                                                                                                                                                                 |
| GET, POST | `/policy/rules/new`, `/policy/rules/duplicate`, `/policy/rules/new/copy`, `/policy/rules/preview`, `/policy/rules/save`, `/policy/rules/delete`, `/policy/rules/value-control`, `/policy/rules/order`, `/policy/rules/enable`, `/policy/rules/disable`, `/policy/rules/revert/{id}`, `/policy/io/import` | admin                             | rule editor and list operations (§14.4)                                                                                                                                                                                                                                                                              |
| POST      | `/policy/fix`, `/policy/fix/apply`                                                                                                                                                                                                                                                                       | admin                             | Make compliant: preview, apply                                                                                                                                                                                                                                                                                       |
| GET, POST | `/devices/new`                                                                                                                                                                                                                                                                                           | admin                             | "Add devices" (#410): one page with four boxes (Discovered, Network scan, Add manually, Import from TasmoAdmin); POST adds one device by address. `?proposal=`, `?address=`, `?port=` prefill the manual box; `?state=`, `?sort=`, `?dir=`, `?ignored=1` shape the Discovered list; `?why=` looks one device up (#713)                                   |
| GET, POST | `/discovery/scan`, `/discovery/scan/{id}`, `/discovery/scan/{id}/stream`, `/discovery/scan/{id}/cancel`, `/discovery/proposals/...` (accept, dismiss, undismiss, one or many)                                                                                                                            | admin                             | network scan results (live while it runs) and the actions of the Discovered box. The tab routes `GET /discovery` and `GET /import/tasmoadmin` are gone (0.27.0, no redirects)                                                                                                                                        |
| POST, GET | `/import/tasmoadmin`, `/import/tasmoadmin/apply`, `/import/tasmoadmin/report.csv`                                                                                                                                                                                                                        | admin                             | web import: the Import box posts here (htmx fragments), the report downloads                                                                                                                                                                                                                                         |
| GET, POST | `/devices/backups` (+ `/stepup`)                                                                                                                                                                                                                                                                         | GET operator, POST admin          | Devices, Backups (#409): last scheduled run, device table with filter chips (`?filter=none                                                                                                                                                                                                                           | old | failed | skipped`, `sort`, `dir`), storage, schedule (saved with the page's own save bar, which shows only while the form has unsaved changes, #949), backups of deleted devices, unreferenced files; "Back up all now" and "Back up selected" post the ids to `/devices/bulk/backup`. Operators see it read-only; moved from Settings, Backups. Every device name in the table is followed by the device ID as a muted `#N` (outside the link, not part of the name, its title or a label, #993). The page is live (#950): it opens the event stream, the status bar refreshes, and the device table reloads itself (with Last run, Storage, the Prune now dialog and, for an admin, the ids "Back up all now" posts, out of band, from the page's own address with filter, sort and dir) on `backup`, `device` and `resync`, asking again after the last event of a burst; ticked rows stay, and the reload waits while a row menu, the Storage fold or a dialog is open. The schedule, the deleted devices' backups and the unreferenced files are never swapped. Bulk cleanup (#622), admin only, each a POST with `confirm=1` and a GET twin that is the confirmation page without JavaScript: `/devices/backups/prune` (Prune now: the ids of the runs the Storage list shows; the server cuts them to what the plan allows now, never a device's newest run, a kept run or one an unfinished restore uses, and leaves devices whose last run failed or was skipped alone), `/devices/backups/orphans/delete` (ids; never a kept run, never a current device's backup) and `/devices/backups/unreferenced/delete` (paths; only what a new scan still lists). One audit entry per action |
| GET, POST | `/settings/system` (+ `/stepup`, `/mqtt-test`, `/sso-test`, `/reset`)                                                                                                                                                                                                                                    | admin                             | System settings (§16); the page's save bar shows only while the form has unsaved changes, so it covers no card (#990)                                                                                                                                                                                                                                                                                                |
| GET, POST | `/settings/system/snapshots` (+ `/stepup`), POST `/settings/system/snapshot`, POST `/settings/system/snapshots/create`, GET, POST `/settings/system/snapshots/{name}/download`, `/delete`                                                                                                                                                          | admin                             | System, Snapshots (#410): database snapshot download, stored snapshots, schedule (saved with the page's own save bar, which shows only while the form has unsaved changes, #990), restore instructions; moved from Settings, Backups. "Create snapshot now" (#921) posts to `/create`                                                                                                                                            |
| GET, POST | `/setup`, `/setup/mqtt-test`, `/setup/finish`                                                                                                                                                                                                                                                            | admin                             | first-run wizard                                                                                                                                                                                                                                                                                                     |
| GET, POST | `/setup/account`                                                                                                                                                                                                                                                                                         | setup link                        | set the initial admin password                                                                                                                                                                                                                                                                                       |
| GET       | `/audit`                                                                                                                                                                                                                                                                                                 | admin                             | audit log                                                                                                                                                                                                                                                                                                            |
| GET       | `/about`                                                                                                                                                                                                                                                                                                 | admin                             | version, decoder version, effective config (secrets masked)                                                                                                                                                                                                                                                          |
| GET       | `/whats-new`, `/nav`, `/styleguide`, `/events`                                                                                                                                                                                                                                                           | operator                          | What's new, navigation page, style guide (sample data), SSE stream (health, job, device, backup)                                                                                                                                                                                                                     |
| POST      | `/prefs/{key}`                                                                                                                                                                                                                                                                                           | operator                          | per-user UI preference                                                                                                                                                                                                                                                                                               |
| GET       | `/account`                                                                                                                                                                                                                                                                                               | signed-in user                    | own account: profile, how they sign in                                                                                                                                                                                                                                                                               |
| POST      | `/account/password`, `/account/profile`                                                                                                                                                                                                                                                                  | local user                        | change own password (current one required), profile                                                                                                                                                                                                                                                                  |
| GET       | `/users`, `/users/new`, `/users/{name}`, `/users/principal/{subject...}`                                                                                                                                                                                                                                 | admin                             | all principals and their detail; no step-up for viewing (header admins: view only)                                                                                                                                                                                                                                   |
| POST      | `/users`, `/users/stepup`, `/users/local/{name}`, `/users/sessions`                                                                                                                                                                                                                                      | admin + step-up (password or IdP) | local user management, End sessions for SSO users                                                                                                                                                                                                                                                                    |

Every other path answers with the app's own not-found page. (`GET /jobs` and `/updates/{batch}`
no longer exist; the transactions pages replaced them.)

Firmware listener: `GET|HEAD /fw/<release>/<filename>`, `GET|HEAD /c/<key>/fw/<release>/<filename>` (uploaded
firmware). Everything else is 404.

A JSON API for automation (with API tokens) is out of scope until there is a consumer.
Home Assistant already gets device data from Tasmota.

---

## 18. UI

Server-rendered pages and HTMX partials in templ (committed generated code, no build step). No inline
scripts. One embedded stylesheet with a light, a dark and an automatic theme (the user's choice is a
stored preference). Usable at phone width. Every page renders inside the shell with a top bar,
a group menu and a sub-navigation (`internal/web/ui/nav.go`; every role sees the same menu, each
page enforces its own permission). Groups and pages:

- **Dashboard** (Overview): six stat tiles (Online, Offline, Unknown, Firmware behind, Policy
  deviations, New devices found; each links to a filtered device list), the fleet health bar,
  "Needs attention" and the newest transactions, with the plans that wait for their start as a line on top
  of that card (#707).
- **Devices**: _List_, _Policy_, _Backups_ (#409, 0.29.0: the fleet's backup state on one page, content width like the Dashboard) and _Add_ (admin; since 0.27.0 one page with four boxes: Discovered and
  Network scan in the wide column, Add manually and Import from TasmoAdmin in the narrow one; the full
  result of a scan is its own page, `/discovery/scan/{id}`). The page heading of the list stays "Devices". The device list is a sortable table with filter chips and a search box (`q`, health,
  family, tag, chip); the selection bar feeds Update, Back up, Send command, Restart and power.
- **Device page** (tabs): Overview (tiles, identity, network facts), Settings and Console where the
  device allows them, Backups, Drift and Replace device (admin).
- **Updates**: _Transactions_ (batches of every job kind, with progress, cancel and retry) and _Firmware_
  (releases, cache, prefetch, upload, release notes). The update flow is: select release + policy
  (+ variant override; the preview shows it, no confirmation checkbox since 2026-10-20), a preview
  table, submit, then the live transaction page (per device: state, stage, attempt, last event,
  elapsed time).
- **System**: _Settings_ (System settings, admin; eight tabs, Backups moved to Devices in 0.29.0), _Snapshots_ (database snapshots, admin; 0.27.0), _Audit log_ (admin), _Users_ (admin), _About_
  (admin: versions, decoder, effective configuration with secrets masked, links to docs) and
  _Styleguide_ (sample data). _What's new_ (parsed from `CHANGELOG.md`) and the account page open from
  the avatar menu; the first-run wizard `/setup` appears for admins until it is finished.

Accessibility: semantic tables and forms, labels, focus styles, no colour-only status
(each badge has text).

---

## 19. Observability

- **Logs**: `log/slog` JSON to stdout (`LOG_FORMAT=json`), fields `time, level, msg,
component, device, job, batch, request_id, remote, user`. No secrets (redactor), no
  device passwords, no backup content. Access logs at info for state-changing requests,
  debug for GETs.
- **Metrics** (`/metrics`, contract with `deploy/grafana/tasmofleet.json`): see the package
  doc of `internal/metrics`. Labels have low cardinality: device names only on
  `tasmofleet_backup_last_success_timestamp_seconds` and the optional
  `tasmofleet_device_*` gauges (fleet of tens; `docs/metrics.md`).
- **Health**: `/healthz` = process alive (blackbox probe). `/readyz` = DB, OIDC discovery,
  listeners.
- **Audit**: database table plus the UI page. Each audit write is also logged at info. When the
  target is a device (`device:<id>`), the line also carries `device` as a number, the key every
  other line uses, so one filter (`"device":45`) finds all lines of a device (#1004); `target` stays.
  No name or topic is added to log lines: both change over time.

---

## 20. Packaging and deployment

- **Image**: multi-stage `Dockerfile`; Go build with `CGO_ENABLED=0`; final
  `gcr.io/distroless/cc-debian13:nonroot` (glibc for the decoder), user 65532. Includes the
  decoder (§12.2). Size about 100 MB with decoder, about 35 MB without. `HEALTHCHECK` uses
  `tasmofleet healthcheck` (no curl in distroless). `VOLUME /data`; ports 8080 (UI, always this container port: map any free host port to it) and 80
  (firmware). Binding port 80 as non-root relies on Docker's default
  `net.ipv4.ip_unprivileged_port_start=0` in the container namespace. If a runtime
  doesn't set it, use `FIRMWARE_LISTEN=:8081` and a port mapping.
- **Home Assistant app image** (#442): Dockerfile target `ha` is the same image as root with the
  `io.hass.type`, `io.hass.version` and `io.hass.arch` labels (the Supervisor's `/data` belongs to root, which
  the non-root user cannot write). `docker build` without `--target` still produces the standard image.
  CI builds it and runs `tools/ha-app-smoke.sh` on every image build; it is not published yet (#444).
- **Registry**: `ghcr.io/hellwand/tasmofleet` and, when the repository's Docker Hub secrets are set,
  `docker.io/nofuturekid/tasmofleet` (overview text `deploy/dockerhub/README.md`), with the same tags
  `<version>` (X.Y.Z) and `latest`. Built and published by GitHub Actions.
- **Unraid**: `deploy/unraid/tasmofleet.xml` (br0, fixed IP, `/data` ->
  a host data directory, env vars, WebUI link to `PUBLIC_URL`). Unraid users take the Community
  Applications template, see docs/dev/releasing.md.
- **Traefik**: example dynamic config in `deploy/traefik/tasmofleet.yml` (router on
  `websecure`, service `http://172.31.12.61:8080`). No forward-auth middleware in OIDC mode;
  an Authentik outpost middleware in header mode.
- **Authentik**: `deploy/authentik/README.md` lists provider settings (confidential client,
  redirect URI, signing key RS256, grant types authorization_code + refresh_token, scopes,
  groups).
- **Keycloak**: `deploy/keycloak/README.md`, checked against a running Keycloak (confidential
  client with PKCE S256, redirect and post-logout URIs, a Group Membership mapper with "Add to ID
  token" and "Full group path" off, `offline_access`, realm URL as issuer without a slash).
- **Grafana**: `deploy/grafana/tasmofleet.json`, 2 graphs per row (layout rule).
- **Data**: back up the data directory like any application data. `secret.key` must be in
  the same backup, or the device passwords cannot be decrypted.

### 20.1 Home Assistant add-on path (M3)

Phase 1 (#442) is in the tree: `deploy/homeassistant/tasmofleet/` (`config.yaml` with ports `8080/tcp` for
the UI with self-signed TLS and local sign-in, `80/tcp` mapped to host 8081 for the firmware server,
`webui`, `backup: cold`, `stage: experimental`; `DOCS.md`, icons, translations), the profile in §16 and the
runbook `docs/runbooks/home-assistant-app.md`. Ingress, the ingress auth mode, the Supervisor client and the
app repository follow in #443 and #444; the list below is the plan for those.

The design is ingress-ready from M1: base path from `X-Ingress-Path` (trusted proxy
`172.30.32.2`), relative links everywhere, SSE under the base path. The CSP allows `frame-ancestors 'self'` only, so
the ingress frame may need a configurable source when M3 is built. M3 adds:

- an add-on repository with `config.yaml` (`ingress: true`, `ingress_port: 8080`, port 80
  mapped for firmware, `map: [addon_config]`. The add-on options are bootstrap settings only
  (`TASMOFLEET_*` by a tiny entry script or `/data/options.json`): trusted proxy, listeners, log
  level. Operational settings (device networks, MQTT, firmware, backups, policy) are set on the
  System settings page; the first start can be preseeded with `tasmofleet settings import`
  from the add-on config directory;
- auth mode `ingress`: trust `X-Remote-User-Id`/`X-Remote-User-Name`/`X-Remote-User-Display-Name`
  from the supervisor proxy only; HA admins get admin, other HA users operator;
- MQTT broker and credentials from the Supervisor services API (`mqtt` service discovery): offered
  as the suggestion in the setup instead of being typed.

---

## 21. Testing strategy and gates

| Layer                           | What                                                                                                                                                                                                                                                                                 | Where                                  | Gate                                                                                                                                                                                 |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Unit                            | pure logic, parsers (fixture-driven), handlers with fakes via `httptest`                                                                                                                                                                                                             | `*_test.go` next to code               | `make test`, `make race`                                                                                                                                                             |
| Fuzz                            | `fwselect.Detect`, `InspectImage`, `tasmoadmin` parsers, `cmdpolicy`, `mqttbus` topic decoding, firmware notes and server paths, settings URL credentials                                                                                                                            | `Fuzz*` with seed corpus from fixtures | the seed corpora run as ordinary tests in `make test`; no timed fuzzing in CI                                                                                                        |
| Mutation                        | `fwselect`, `ota`, `policy`, `cmdpolicy`                                                                                                                                                                                                                                             | gremlins                               | `make mutation` (`MUTATION` in the Makefile): efficacy **≥ 90 % fwselect, ≥ 85 % ota, ≥ 85 % policy, ≥ 90 % cmdpolicy**; the weekly run keeps it, PRs run diff coverage and a revert check instead (`make diff-cover`, `make revert-check`) |
| Integration (tag `integration`) | engine + simulator fleet (all profiles and faults), store on real SQLite, mqttbus + embedded broker, auth against oidcmock, decoder against the real binary (skipped with a reason only if `DECODER_PATH` is missing; CI sets `REQUIRE_DECODER=1`, so there a missing decoder fails) | `*_integration_test.go`                | `make integration`                                                                                                                                                                   |
| e2e                             | real binary + `tasmofleet-devenv` (mixed fleet: MQTT and HTTP-only devices, ESP8266 1 MB staged, ESP32 direct, unknown chip refused), login via mock OIDC with Authentik claims, update flow end to end, header mode via a test proxy, base path `/tasmofleet`, no-MQTT mode         | `e2e/tests/*.spec.ts`                  | `make e2e` (all but quarantined), `make e2e-core` (tag `@core`)                                                                                                                      |
| Static                          | gofmt, go vet, staticcheck, golangci-lint (incl. depguard, exhaustive, gosec, bodyclose, noctx)                                                                                                                                                                                      |                                        | `make check`                                                                                                                                                                         |
| Supply chain                    | govulncheck                                                                                                                                                                                                                                                                          |                                        | `make vuln`                                                                                                                                                                          |

Simulator scenarios (minimum set, each an integration test):
ESP8266 4 MB direct; ESP8266 1 MB staged (with OtaUrl ending as minimal and being restored);
Tasmota auto-fallback (`ota_loader`) continuing on its own; ESP32 direct; ESP32-C3 and S3
selection; unknown hardware refused; wrong-family artifact refused (header) ; safeboot
artifact refused; variant change refused without flag; reboot gap longer than
UpgradeAccept; download failure ("Failed File Not Found (404)") with retry then fail;
device never comes back; process restart in every non-terminal state (resume never
re-sends Upgrade); lost Upgrade response; another tool changed OtaUrl; auth failure;
HTTP-only device without MQTT; flapping LWT; `{"Command":"Unknown"}` for Status on
minimal.

CI (`.github/workflows/`): one workflow per trigger. `pr.yml` runs the FAST tier on every PR (lint, unit and
integration tests from the cache, build, `@core` e2e in two shards, safety race and diff coverage when a `SAFETY` package
changed, a revert check for Go changes (the changed tests fail without the production change); the `required` job gates the merge). `main.yml` publishes `:edge` and saves the Go caches (lint, unit and integration
tests, no e2e). `release.yml` runs on tags: the FAST tier plus the race tests of every `SAFETY` package, and publishes the
image, the GitHub release and the demo site only after it passed. `nightly.yml` runs the FULL tier (every e2e test,
race on all packages, uncached tests, full mutation, the Home Assistant app in a Supervisor) and failure-reports.
Details: docs/dev/ci-tiers.md.

---

## 22. Migration from TasmoAdmin

1. Deploy tasmofleet next to TasmoAdmin (new IP). If a firewall restricts devices to the
   OTA server, allow the new address (IoT -> TCP 80) or plan step 6.
2. `tasmofleet import tasmoadmin /path/to/tasmoadmin/data --dry-run`, then run it for real.
   Check probes and MAC merge.
3. Run backups of all devices (manual batch), then compare a few decoded backups with
   the fleet policy.
4. Do the first OTA on one ESP8266EX plug and one ESP8285 lamp (both staged), then one
   ESP32 (direct). Check that `OtaUrl` is restored.
5. Run both in parallel for a while (TasmoAdmin read-only).
6. Retire TasmoAdmin. Optionally move tasmofleet to its address so a firewall rule stays
   unchanged.

Known leftover: devices updated by TasmoAdmin store
`http://tasmoadmin.example.internal:80/data/firmwares/tasmota-minimal.bin.gz` as OtaUrl
(OBS). tasmofleet records whatever it finds as `OriginalOtaURL`. The M2 policy view can
offer to set a clean value fleet-wide.

---

## 23. Milestones and exit criteria

**M1 core**:
Exit criteria: `make ci` green; e2e covers login (OIDC mock + header mode +
local accounts incl. first run, standalone mode without proxy/IdP), import, staged and direct OTA on simulators, backup with decoder, no-MQTT
mode; mutation thresholds met; image published to the registry; a manual staged update
on a real fleet.

**M2 parity**: device list live state + toggle, console, settings editor, bulk commands

- templates, restart, autoscan + MQTT discovery, policy/drift view, backup diff +
  restore, custom firmware upload, web import. Planned and not built: the development channel, notifications,
  fleet settings templates (§8.5, §14.5, §14.2).
  Exit criterion: TasmoAdmin can be retired.

**M3**: Home Assistant add-on with ingress auth, public GitHub repository
(`github.com/hellwand/tasmofleet`, the module path already matches), GHCR image,
`.github/workflows` copy, fixtures re-checked for publication.

---

## 24. Risks and open points

- **Minimal behaviour is derived from source, not observed** (no device was on minimal
  during capture). The simulator follows the source analysis. The first real staged update
  must be watched, and the simulator adjusted if needed.
- **Image tags of ESP32 variant and chip builds** (`tasmota32c3`, `tasmota32-sensors`, ...)
  are partly SRC/assumed. Unknown tags refuse (safe), and each newly seen tag gets a
  fixture.
- **solo1 hardware strings** need confirmation. Until then, unknown ESP32 single-core
  strings refuse.
- **decode-config CLI flags** are verified only in the integration test against the
  pinned binary.
- **Port 80 as non-root** depends on the container runtime's unprivileged port setting
  (§20).
- **Free flash on ESP8266** (`Status 4 Free`) semantics for gzip OTA: the staged/direct
  threshold is conservative, and a real update verifies it.
