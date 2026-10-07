-- Initial schema (specification §9.3).
-- Times are Unix milliseconds (INTEGER, UTC), IDs are UUIDv7 text, see ADR 0007.

-- +goose Up
CREATE TABLE users (
    id             TEXT    NOT NULL PRIMARY KEY,
    username       TEXT    NOT NULL COLLATE NOCASE UNIQUE,
    password_hash  TEXT    NOT NULL,
    quota_json     TEXT    NOT NULL,
    disabled       INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
    config_version TEXT    NOT NULL,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
) STRICT;

CREATE TABLE sessions (
    id           BLOB    NOT NULL PRIMARY KEY, -- SHA-256 of the cookie token
    user_id      TEXT    NOT NULL REFERENCES users (id),
    csrf_secret  BLOB    NOT NULL,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX sessions_user_id ON sessions (user_id);
CREATE INDEX sessions_expires_at ON sessions (expires_at);

CREATE TABLE jobs (
    id               TEXT    NOT NULL PRIMARY KEY,
    owner_id         TEXT    NOT NULL REFERENCES users (id),
    nzb_name         TEXT    NOT NULL,
    nzb_digest       BLOB    NOT NULL, -- SHA-256 of the decompressed NZB XML
    status           TEXT    NOT NULL CHECK (status IN ('queued', 'importing', 'ready', 'failed', 'deleted')),
    failure_code     TEXT,
    failure_message  TEXT,
    backend_ref_json TEXT,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    ready_at         INTEGER,
    failed_at        INTEGER,
    expires_at       INTEGER,
    CHECK ((status = 'failed') = (failure_code IS NOT NULL)),
    CHECK (status = 'deleted' OR expires_at IS NOT NULL)
) STRICT;

-- One live job per owner and NZB; Failed and Deleted jobs are not duplicates.
CREATE UNIQUE INDEX jobs_owner_digest_live ON jobs (owner_id, nzb_digest)
    WHERE status NOT IN ('failed', 'deleted');
CREATE INDEX jobs_owner_created ON jobs (owner_id, created_at DESC);
CREATE INDEX jobs_status ON jobs (status);

CREATE TABLE job_files (
    job_id       TEXT    NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    rel_path     TEXT    NOT NULL,
    size         INTEGER NOT NULL CHECK (size >= 0),
    content_type TEXT    NOT NULL,
    PRIMARY KEY (job_id, rel_path)
) STRICT, WITHOUT ROWID;

-- Transactional outbox and SSE replay buffer. AUTOINCREMENT keeps IDs from
-- being reused after pruning, so a stale Last-Event-ID is always detectable.
CREATE TABLE job_events (
    id           INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    job_id       TEXT    NOT NULL,
    owner_id     TEXT    NOT NULL,
    type         TEXT    NOT NULL,
    payload_json TEXT    NOT NULL,
    created_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX job_events_owner_id ON job_events (owner_id, id);
CREATE INDEX job_events_created_at ON job_events (created_at);

CREATE TABLE usage_daily (
    user_id      TEXT    NOT NULL REFERENCES users (id),
    day          TEXT    NOT NULL, -- UTC date, YYYY-MM-DD
    bytes_served INTEGER NOT NULL DEFAULT 0 CHECK (bytes_served >= 0),
    PRIMARY KEY (user_id, day)
) STRICT, WITHOUT ROWID;

CREATE TABLE nzb_blobs (
    job_id  TEXT NOT NULL PRIMARY KEY REFERENCES jobs (id) ON DELETE CASCADE,
    content BLOB NOT NULL
) STRICT;

-- +goose Down
-- Forward-only: releases never roll back (spec §9.2).
SELECT 'down migrations are not supported';
