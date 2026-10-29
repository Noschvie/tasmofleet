# synthetic-esp32s2 (synthetic)

Not a device capture. `profile.json` names a real capture (`base`) and text
replacements the simulator applies to every file of it, so the profile
behaves like an ESP32-S2. Used only by `internal/simulator`, the OTA
integration tests and the devenv fleet (#88).
