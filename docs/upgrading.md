# Upgrading to 0.17

**Action may be required.** From 0.17 the UI listens on **container port 8080 in every mode** (it
was 8443 in the zero-configuration TLS mode), and `TASMOFLEET_TLS_SELF_SIGNED` is gone (TLS
follows the mode). Installs that were saved with an explicit `TASMOFLEET_LISTEN` keep working
unchanged; installs that relied on the old automatic `:8443` do not. Find yours below.

Background: [#152](https://github.com/nofuturekid/tasmofleet/issues/152). The full variable
reference is [configuration.md](configuration.md).

## What happens if you do nothing

| Your install                                                                                                                                                                                                                                                                              | After pulling 0.17                                                                                                                                                                         |
| ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **Unraid, Community Applications template** with the shipped defaults (`8443` to `8443`, UI listen `:8443`, Self-signed TLS `true`)                                                                                                                                                       | Works as before: same address, same self-signed HTTPS. The start log warns that `TASMOFLEET_TLS_SELF_SIGNED` no longer exists.                                                             |
| **Unraid behind a reverse proxy** (UI listen `:8080`, Self-signed TLS `false`, Trusted proxies set)                                                                                                                                                                                       | Works as before: plain HTTP on `:8080`. On `br0` or a custom IP the saved `:8080` is kept.                                                                                                 |
| `docker run` or Compose with `-p 8443:8443` **and** `TASMOFLEET_LISTEN=:8443` (the README example of 0.16)                                                                                                                                                                                | Works as before. The start log warns about `TASMOFLEET_TLS_SELF_SIGNED` if you set it.                                                                                                     |
| **Any** container with an `8443:8443` mapping and **no** (or an **empty**, `docker run`/Compose) `TASMOFLEET_LISTEN`, including an Unraid container whose UI listen **row was deleted** from its template (an emptied field falls back to the template default `:8443` and keeps working) | **The UI is unreachable on the old port**: an empty value counts as unset (before: `:8443`, now `:8080`). Change the container port to 8080 (`-p 8443:8080`).                              |
| Any install with `TASMOFLEET_TLS_SELF_SIGNED=true` **and** `TASMOFLEET_TRUSTED_PROXIES` (or `ALLOW_INSECURE_HTTP=true`)                                                                                                                                                                   | The listener now serves **plain HTTP** (before: TLS). Point the proxy at `http://` instead of `https://`, or remove the proxies to get the self-signed certificate. The start log says so. |
| Outside a container, no `TASMOFLEET_LISTEN`, zero-configuration TLS                                                                                                                                                                                                                       | Listens on `:8080` instead of `:8443`. Set `TASMOFLEET_LISTEN=:8443` to keep the old port.                                                                                                 |

Everything else (data, users, settings, the firmware port 80) is untouched.

## Steps

**Unraid (Community Applications).** An existing container keeps the values you saved; the changed
template does not rewrite them, so nothing has to be edited. To adopt the new layout (optional):

1. Docker tab, `tasmofleet`, Edit.
2. Delete the variables **UI listen**, **Self-signed TLS** and **Firmware listen** (Advanced view).
3. Web UI port: set the container port to `8080` (host port may stay `8443`).
4. Apply. On a bridge network the UI is then at `https://<server>:8443/` as before. Behind a reverse proxy keep
   **Trusted proxies** set; the same port then serves plain HTTP.

On `br0` or a custom IP there is no port mapping. The container listens on the saved UI listen
port (8443 with the template defaults, 8080 in the proxy setup). If you delete UI listen, it
listens on 8080 (UI) and 80 (firmware) of its IP, so update bookmarks and the reverse proxy
upstream.

**docker run.** Replace `-p 8443:8443 -e TASMOFLEET_LISTEN=:8443 -e TASMOFLEET_TLS_SELF_SIGNED=true`
with `-p 8443:8080`.

**Compose.** Under `ports:` change `"8443:8443"` to `"8443:8080"` and remove `TASMOFLEET_LISTEN`
and `TASMOFLEET_TLS_SELF_SIGNED` from `environment:`.

**Reverse proxy.** Upstream stays `http://<container>:8080`. Nothing changes if you already
run with `TASMOFLEET_TRUSTED_PROXIES`.

## How the scheme is chosen now

See [How TLS is chosen](configuration.md#how-tls-is-chosen): trusted proxies mean plain HTTP,
a certificate means TLS with it, `ALLOW_INSECURE_HTTP=true` means plain HTTP, and with none of
them a self-signed certificate. The port never changes with the mode.

# Upgrading to 0.18: backup file layout

Backups were stored as `backups/<device-id>/<stamp>-<kind>.<ext>`. From 0.18 they are named after the
device (hostname, else inventory name): `backups/<hostname>/<stamp>_<hostname>.dmp`, `.snapshot.json`
and `.decoded.json`. Background: [#129](https://github.com/nofuturekid/tasmofleet/issues/129).

**You do not have to do anything.** At every start tasmofleet moves the old files:

- A file is moved only if its SHA-256 equals the one in the index; then it is renamed (same
  volume, no copy) and the index is updated. Nothing is deleted. A file that does not match, is
  missing or would collide with another one stays where it is and is logged as a warning
  ("backup layout: left in place").
- If the container stops in the middle, the next start completes the move.
- Empty `backups/<number>/` folders are removed afterwards. The kept backups of deleted devices move
  too (0.19 and later; 0.18.0 left them in `backups/<number>/`): into a folder named after the
  deleted device, or `orphan-<device-id>` when no name is stored. They never share a folder with
  another device's backups.
- A backup whose file you deleted by hand cannot be moved. It is counted in the start-up log line
  ("backup layout", `files_missing`), marked "File missing" in the backup lists and can be deleted
  there.
- Two devices with the same hostname get `<hostname>` and `<hostname>_<device-id>`.
- If a device's hostname changes later, new backups go to the new folder; old ones stay put and
  remain listed and restorable.
- A **downgrade to 0.17** still works: the index stores each file's path.

If you have scripts or a backup job that read `backups/<device-id>/`, change them to follow the new
folders (or read the paths from the database).
