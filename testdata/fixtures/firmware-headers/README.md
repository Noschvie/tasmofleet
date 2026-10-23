# Firmware image headers

First 32 bytes of the Tasmota v16.0.0 release images from http://ota.tasmota.com
(fetched 2026-10-18). For `*.bin.gz.hdr` the bytes are taken from the
decompressed image, which is what `fwselect.InspectImage` reads.

ESP image header: byte 0 magic `0xE9`. ESP32 app images carry an extended
header: byte 8 = `0xEE` (WP pin "disabled"), bytes 12-13 = `chip_id`
(little endian). Observed chip ids:

| file | chip_id |
|---|---|
| tasmota32, tasmota32solo1, tasmota32-safeboot | 0x0000 (ESP32) |
| tasmota32s2 | 0x0002 |
| tasmota32c3 | 0x0005 |
| tasmota32s3 | 0x0009 |
| tasmota32c2 | 0x000C |
| tasmota32c6 | 0x000D |
| tasmota32p4 | 0x0012 |
| tasmota32c61 | 0x0014 |
| tasmota32c5 | 0x0017 |

ESP8266 images (all variants identical in the first bytes) have no extended
header: byte 8 is the first segment's load address (`0x00`). solo1 and the
classic ESP32 build share chip_id 0; they are told apart by file name and
`StatusFWR.Hardware`, never by the header.
