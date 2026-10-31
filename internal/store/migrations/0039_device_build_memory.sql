-- 0039_device_build_memory: the chip build (ESP32-S2: "s2" or "s2cdc") a person explicitly chose for
-- this device in the update preview, so the next preview can preselect it (#186). NULL = none
-- remembered. Written only by that explicit choice (Start update) and cleared by "Forget"; nothing
-- the device reports may fill it, because both S2 builds report the same image name.
ALTER TABLE devices ADD COLUMN build_memory TEXT CHECK (build_memory IS NULL OR build_memory IN ('s2', 's2cdc'));
