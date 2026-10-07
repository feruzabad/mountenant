-- name: UserByUsername :one
SELECT * FROM users WHERE username = ?;

-- name: UserByID :one
SELECT * FROM users WHERE id = ?;

-- name: AllUsers :many
SELECT * FROM users ORDER BY username;

-- name: InsertUser :exec
INSERT INTO users (id, username, password_hash, quota_json, disabled, config_version, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateUser :exec
UPDATE users
SET password_hash = ?, quota_json = ?, disabled = ?, config_version = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteSessionsForUser :exec
DELETE FROM sessions WHERE user_id = ?;

-- name: InsertSession :exec
INSERT INTO sessions (id, user_id, csrf_secret, created_at, last_seen_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: SessionByID :one
SELECT * FROM sessions WHERE id = ?;

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = ? WHERE id = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = ?;

-- name: DeleteInactiveSessions :execrows
DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?;
