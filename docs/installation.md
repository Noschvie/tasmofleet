# Installing TasmoFleet

The short version is in the [README](../README.md#quick-start-docker); this page has the details.

## Images and architectures

The image is published to two registries under the same tags (`latest`, `X.Y.Z` and `edge`, the
current `main`): `nofuturekid/tasmofleet` on Docker Hub and `ghcr.io/nofuturekid/tasmofleet`. Use
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
