# Real upstream captures for the firmware package

One-off, read-only GET requests, fetched 2026-10-18:

| file | source |
|---|---|
| `tasmota-release.html` | https://ota.tasmota.com/tasmota/release/ (ESP8266 listing, release 16.0.0 "Theo") |
| `tasmota32-release.html` | https://ota.tasmota.com/tasmota32/release/ (ESP32 listing, release 16.0.0) |
| `github-releases.json` | https://api.github.com/repos/arendst/Tasmota/releases?per_page=10, reduced with jq to the two newest releases (v16.0.0, v15.6.0) and to the fields the parser reads (`tag_name`, `draft`, `prerelease`, `published_at`, assets' `name`, `size`, `browser_download_url`); release notes dropped |

Facts the parsers rely on (and that the hand-written fixtures got wrong):

- The listings are not an Apache index. The version is in an `<h2>` ("Release binaries for Tasmota firmware 16.0.0 Theo on ESP8266").
- File links in the table are **unquoted** (`<a href=tasmota-4M.bin.gz>`), other links are quoted.
- The ESP8266 table also links the uncompressed `*.bin` ("Initial Firmware", serial flashing). They are not OTA targets.
- The full GitHub reply for `per_page=10` is about 5.6 MB (release notes, 260-290 assets per release).
