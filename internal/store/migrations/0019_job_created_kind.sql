-- 0019_job_created_kind: a "Send command" job is sent with the role its creator
-- has when the step runs. Subjects are not unique across sign-in methods (a local
-- account, a header user and an OIDC subject can spell the same), so the job
-- also keeps the method the creator signed in with; an empty value (every job
-- before this migration) never resolves to a principal.
ALTER TABLE ota_jobs ADD COLUMN created_kind TEXT NOT NULL DEFAULT '';
