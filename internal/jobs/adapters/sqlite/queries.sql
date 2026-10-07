-- name: InsertJob :exec
INSERT INTO jobs (id, owner_id, nzb_name, nzb_digest, status, failure_code, failure_message, backend_ref_json,
                  created_at, updated_at, ready_at, failed_at, expires_at, next_check_at, attempts, backend_removed_at, version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1);

-- name: UpdateJob :execrows
UPDATE jobs
SET status = ?, failure_code = ?, failure_message = ?, backend_ref_json = ?, updated_at = ?, ready_at = ?,
    failed_at = ?, expires_at = ?, next_check_at = ?, attempts = ?, backend_removed_at = ?, version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(expected_version);

-- name: LiveJobByDigest :one
SELECT * FROM jobs
WHERE owner_id = ? AND nzb_digest = ? AND status IN ('queued', 'importing', 'ready');

-- name: JobByOwner :one
SELECT * FROM jobs WHERE id = ? AND owner_id = ?;

-- name: JobByID :one
SELECT * FROM jobs WHERE id = ?;

-- name: ListJobs :many
SELECT * FROM jobs
WHERE owner_id = sqlc.arg(owner_id)
  AND status != 'deleted'
  AND (sqlc.narg(status) IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(cursor_created) IS NULL
       OR created_at < sqlc.narg(cursor_created)
       OR (created_at = sqlc.narg(cursor_created) AND id < sqlc.narg(cursor_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit);

-- name: CountJobs :one
SELECT
    CAST(COALESCE(SUM(status IN ('queued', 'importing')), 0) AS INTEGER) AS active,
    CAST(COALESCE(SUM(status != 'deleted'), 0) AS INTEGER) AS total
FROM jobs WHERE owner_id = ?;

-- name: DueJobs :many
SELECT * FROM jobs
WHERE next_check_at IS NOT NULL AND next_check_at <= ?
ORDER BY next_check_at
LIMIT ?;

-- name: ExpiredJobs :many
SELECT * FROM jobs
WHERE status != 'deleted' AND expires_at <= ?
ORDER BY expires_at
LIMIT ?;

-- name: KnownJobIDs :many
SELECT id FROM jobs WHERE status != 'deleted';

-- name: JobFiles :many
SELECT * FROM job_files WHERE job_id = ? ORDER BY rel_path;

-- name: InsertJobFile :exec
INSERT INTO job_files (job_id, rel_path, size, content_type) VALUES (?, ?, ?, ?);

-- name: DeleteJobFiles :exec
DELETE FROM job_files WHERE job_id = ?;

-- name: InsertNZB :exec
INSERT INTO nzb_blobs (job_id, content) VALUES (?, ?);

-- name: NZBBlob :one
SELECT content FROM nzb_blobs WHERE job_id = ?;

-- name: DeleteNZB :exec
DELETE FROM nzb_blobs WHERE job_id = ?;

-- name: InsertEvent :exec
INSERT INTO job_events (job_id, owner_id, type, payload_json, created_at) VALUES (?, ?, ?, ?, ?);

-- name: PurgeNZBs :execrows
DELETE FROM nzb_blobs
WHERE job_id IN (SELECT id FROM jobs WHERE status IN ('failed', 'deleted') OR backend_ref_json IS NOT NULL);
