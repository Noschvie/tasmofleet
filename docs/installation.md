# Installing TasmoFleet

The short version is in the [README](../README.md#quick-start-docker); this page has the details.

## Images and architectures

The image is published to two registries under the same tags (`latest`, `X.Y.Z` and `edge`, the
current `main`): `nofuturekid/tasmofleet` on Docker Hub and `ghcr.io/hellwand/tasmofleet`. Use
either name; to pull from ghcr.io instead, put `ghcr.io/` in front of the image name.

Every tag is a multi-arch image: `linux/amd64` and `linux/arm64` (Raspberry Pi 4/5 and other 64-bit
ARM boards). Docker picks the right one. Both include the backup decoder
([decode-config](https://github.com/tasmota/decode-config), LGPL-3.0, run as a separate program). On
arm64 it is an unofficial build of exactly upstream v16.0.0 plus the merged aarch64 workflow change
([tasmota/decode-config#127](https://github.com/tasmota/decode-config/pull/127)), made in
[nofuturekid/tasmota-decode-config](https://github.com/nofuturekid/tasmota-decode-config/releases/tag/v16.0.0-arm64);
the Dockerfile switches to upstream's asset once a release ships aarch64. 32-bit ARM is not built.

The UI always listens on container port 8080; `8443:8080` maps container port 8080 to host port 8443, which you can choose freely. Upgrading
from 0.16 or older with a `-p 8443:8443` mapping? Read [Upgrading to 0.17](upgrading.md) first.

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
    image: nofuturekid/tasmofleet:latest
    container_name: tasmofleet
    restart: unless-stopped
    ports:
      - "8443:8080" # UI (HTTPS, self-signed; container port is always 8080)
      - "80:80" # firmware for the devices (plain HTTP)
    environment:
      TZ: "Europe/Berlin" # local time instead of UTC (logs, times the app formats)
    volumes:
      - tasmofleet-data:/data
volumes:
  tasmofleet-data:
```

The container runs as UID 65532. A named volume (as above) works out of the box. For a bind
mount (`-v /srv/tasmofleet:/data`) make the directory writable for that user first, otherwise
Docker creates it as root and TasmoFleet refuses to start with `data dir /data is not writable`:

```sh
mkdir -p /srv/tasmofleet && chown -R 65532:65532 /srv/tasmofleet
```

Alternatively set an explicit `user: "<uid>:<gid>"` in Compose (or `--user`) that owns the
directory. Set `TZ` (e.g. `Europe/Berlin`) to get local time instead of UTC
([what it changes](configuration.md#time-zone)).

With Compose, the setup link is in `docker compose logs tasmofleet`.

**Unraid**: a template is in [deploy/unraid/tasmofleet.xml](../deploy/unraid/tasmofleet.xml) (copy
it to `/boot/config/plugins/dockerMan/templates-user/`); it runs plain HTTP behind a reverse
proxy with its own IP on `br0` (UI on port 8080, firmware on port 80 of that IP). The Community
Applications template uses the `bridge` network instead, with the port mappings as the only
place for ports. Upgrading an existing install: [docs/upgrading.md](upgrading.md).

## Native install (without a container)

Every release also ships static binaries on its [GitHub release page](https://github.com/hellwand/tasmofleet/releases):
`tasmofleet_X.Y.Z_linux_amd64.tar.gz` and `tasmofleet_X.Y.Z_linux_arm64.tar.gz`, each with the binary, `LICENSE`,
`README.md` and a systemd unit ([deploy/systemd/tasmofleet.service](../deploy/systemd/tasmofleet.service)), plus
`SHA256SUMS`. It is the same program as the image, built without C libraries, so it runs on any 64-bit Linux with
systemd. There are no `.deb` or `.rpm` packages and no 32-bit ARM build.

Install it as a service of its own user (replace `X.Y.Z` with the release, `amd64` with `arm64` on a Raspberry Pi 4/5):

```sh
v=X.Y.Z arch=amd64
curl -fLO "https://github.com/hellwand/tasmofleet/releases/download/v$v/tasmofleet_${v}_linux_${arch}.tar.gz"
curl -fLO "https://github.com/hellwand/tasmofleet/releases/download/v$v/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS
tar -xzf "tasmofleet_${v}_linux_${arch}.tar.gz" && cd "tasmofleet_${v}_linux_${arch}"
sudo useradd --system --user-group --home-dir /var/lib/tasmofleet --shell /usr/sbin/nologin tasmofleet
sudo install -m 0755 tasmofleet /usr/local/bin/tasmofleet
sudo install -m 0644 tasmofleet.service /etc/systemd/system/tasmofleet.service
sudo systemctl daemon-reload
sudo systemctl enable --now tasmofleet
sudo journalctl -u tasmofleet | grep 'setup link'
```

The unit does not create the user (`useradd` above does); systemd creates the data directory `/var/lib/tasmofleet`,
readable only by that user, at the first start. Then open `https://<host>:8080` followed by the `/setup/account?token=...`
path from that log line, as in the container setup above.

- **Ports**: the UI listens on 8080 (`TASMOFLEET_LISTEN`), the firmware server for the devices on 80
  (`TASMOFLEET_FIRMWARE_LISTEN`; the unit grants `CAP_NET_BIND_SERVICE` for it, nothing else runs as root). Set the URL
  the devices use on the System settings page, as with the image.
- **Settings**: put variables ([configuration](configuration.md)) into `/var/lib/tasmofleet/tasmofleet.env`, or into a
  drop-in (`sudo systemctl edit tasmofleet`, `Environment=` lines). A secret given as `_FILE` must be readable by the
  `tasmofleet` user and must not be under `/home` (the unit hides it). After a TLS certificate change,
  `sudo systemctl reload tasmofleet` re-reads the files.
- **Command line** (accounts, import, settings): run it as the service user against the same data directory, e.g.
  `sudo -u tasmofleet TASMOFLEET_DATA_DIR=/var/lib/tasmofleet tasmofleet user list`.
- **Upgrade**: download and check the new archive as above, `sudo install -m 0755 tasmofleet /usr/local/bin/tasmofleet`
  (and the unit, followed by `sudo systemctl daemon-reload`), then `sudo systemctl restart tasmofleet`. Database
  migrations run at the start, as in the container; read the release's upgrade notes first.
- **Uninstall**: `sudo systemctl disable --now tasmofleet`, remove `/etc/systemd/system/tasmofleet.service` (and a
  drop-in directory `tasmofleet.service.d` if you made one) and `/usr/local/bin/tasmofleet`, `sudo systemctl
daemon-reload`, `sudo userdel tasmofleet`. `/var/lib/tasmofleet` stays on purpose: it holds the database with the
  device credentials, `secret.key` and the backups. Delete it (`sudo rm -rf /var/lib/tasmofleet`) as a separate step,
  once you no longer need them.

### Backup decoder for a native install

The archive does not contain [decode-config](https://github.com/tasmota/decode-config), the program the image uses to
decode backups. Without it TasmoFleet works and keeps the raw configuration dump of every backup only (the start log says
`decode-config not found`). To get decoded backups, install it yourself at `/usr/local/bin/decode-config` (the default
of `TASMOFLEET_DECODER_PATH`), the version the image uses: the `DECODER_*` build arguments at the top of the
[Dockerfile](../Dockerfile) of your release name the version, the download and its SHA-256.

- **amd64**: upstream's release asset `decode-config-ubuntu-24.04.tar.gz`. It is a PyInstaller build that needs glibc
  and `libz.so.1` (zlib) on the host, so it does not run on a musl system such as Alpine.
- **arm64**: the unofficial build named by `DECODER_ARM64_URL` (see [Images and architectures](#images-and-architectures)).

```sh
curl -fLo decode-config.tar.gz "<the download URL from the Dockerfile>"
sha256sum decode-config.tar.gz   # must equal the SHA-256 from the Dockerfile
sudo tar -xzf decode-config.tar.gz -C /usr/local/bin decode-config
sudo chmod 0755 /usr/local/bin/decode-config
sudo systemctl restart tasmofleet
```

TasmoFleet runs it with `PATH=/usr/local/bin:/usr/bin:/bin` and `TMPDIR=<DATA_DIR>/tmp`: it unpacks itself there and
runs from there, so the data directory must not be mounted `noexec`. The start log names the decoder version it found.

## MQTT broker (optional)

TasmoFleet works without MQTT. With a broker, live state and every operation on a device that is online there go over
MQTT first (System, MQTT).

> [!CAUTION]
> **Set up an ACL on your MQTT broker before you use TasmoFleet's MQTT features.** MQTT is the first way TasmoFleet
> talks to a device that is online on its broker (reads, changes, backups and restores). The broker hands every command,
> every password in it and the complete configuration dump of every backed-up or restored device to **every client
> that may subscribe to the device's topics**: Home Assistant, Node-RED, MQTT Explorer, other devices. Without an ACL
> that is every client of the broker. A ready, tested rule set is in the [broker ACL guide](mqtt-broker-acl.md);
> note that the Mosquitto app of Home Assistant currently does not enforce an `acl_file` (the guide says what to do).
