-- 0052_backup_attempt_vitals: the evidence of a failed backup (#887).
--
-- heap_kb and uptime_sec are the device's free heap (kB) and uptime (seconds) read with one Status 11 right
-- after the failed dump, when the device had answered it; each attempt carries its own. 0 means not read
-- (no answer, a dump refused before it was sent, or an attempt recorded before this migration). Additive
-- only; the values end with their row (audit retention, or ON DELETE CASCADE with the device).
ALTER TABLE backup_attempts ADD COLUMN heap_kb    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backup_attempts ADD COLUMN uptime_sec INTEGER NOT NULL DEFAULT 0;
