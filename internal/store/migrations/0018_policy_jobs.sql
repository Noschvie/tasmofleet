-- 0018_policy_jobs: "Make compliant" runs as a transaction next to firmware
-- updates. A job has a kind (update or policy); a policy job carries the
-- device commands it sends, one per line, fixed at creation.
ALTER TABLE ota_jobs ADD COLUMN kind TEXT NOT NULL DEFAULT 'update';
ALTER TABLE ota_jobs ADD COLUMN commands TEXT NOT NULL DEFAULT '';
