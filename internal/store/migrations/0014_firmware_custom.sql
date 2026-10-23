-- Custom firmware: images an admin uploads. An upload may carry the same
-- release and file name as an upstream file (a self-built 16.0.0), so
-- artifacts get a "custom" key (empty for upstream files). The old
-- UNIQUE(release, filename) stays: the store writes the release of an upload as
-- "<key>~<release>" (see customPrefix in misc.go), which keeps both apart without
-- rebuilding the table. That is the less invasive option: no copy of the table,
-- one id space for jobs, in-use lookups and delete.
ALTER TABLE firmware_artifacts ADD COLUMN custom TEXT NOT NULL DEFAULT '';

-- The upload an OTA job installs ('' = the upstream release).
ALTER TABLE ota_jobs ADD COLUMN target_custom TEXT NOT NULL DEFAULT '';

-- Provenance of an upload: the image itself is a firmware_artifacts row.
CREATE TABLE firmware_custom (
    artifact_id   INTEGER PRIMARY KEY REFERENCES firmware_artifacts(id) ON DELETE CASCADE,
    label         TEXT    NOT NULL,
    original_name TEXT    NOT NULL,
    uploaded_by   TEXT    NOT NULL,
    uploaded_at   INTEGER NOT NULL
);
