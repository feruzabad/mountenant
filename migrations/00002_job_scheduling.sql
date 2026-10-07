-- Reconciler scheduling (UC-13, UC-14, ADR 0004).
-- next_check_at: when the reconciler next looks at the job (status poll,
--   ready re-check, submit retry or backend cleanup).
-- attempts: consecutive failed attempts, for backoff and alerting.
-- backend_removed_at: when VerifyGone confirmed that a deleted job is gone
--   from the backend; NULL means cleanup is still pending.

-- +goose Up
ALTER TABLE jobs ADD COLUMN next_check_at INTEGER;
ALTER TABLE jobs ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN backend_removed_at INTEGER;

CREATE INDEX jobs_due ON jobs (next_check_at)
    WHERE next_check_at IS NOT NULL;

-- +goose Down
SELECT 'down migrations are not supported';
