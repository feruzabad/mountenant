# Mountenant — Specification

Oct 7, 2026

Revision 2 (Oct 7, 2026): backend integration updated with the phase-2 case study (`docs/feasibility.md`) and ADRs 0002–0005 (`docs/adr/`).
Revision 3 (Oct 7, 2026): results of the backend adapter spike (`docs/spike-backend-adapter.md`): `Ping` on the port, transparent upstream resume (ADR 0006), NzbDav delete idempotency.

## 1. Scope and goals

Mountenant is a multi-user web front door to a single private backend instance (AltMount or NzbDav, interchangeable): users upload NZB files, watch them become available, and download the resulting files through short-lived signed URLs. Mountenant adds what the backends lack (user accounts, per-user isolation, quotas, signed links) and nothing else; NNTP, yEnc, archive handling and streaming stay inside the backend.

### 1.1 Goals

- G1. Authenticated users can submit NZB files and see the status of their own jobs only.
- G2. Completed jobs expose a file listing; each file downloads through a signed, expiring URL with HTTP range (resume) support.
- G3. No local disk is consumed for content; Mountenant streams bytes from the backend to the client.
- G4. Users are provisioned via configuration files and environment variables; there is no admin UI.
- G5. Interactive single-page UI (React + TypeScript) with live job status and a brand logo.
- G6. One deployable artifact: a single Go binary (and container image) that embeds the built frontend.
- G7. Codebase follows domain-driven design, established standards (RFCs, OWASP, OpenAPI) and stays lean.

### 1.2 Non-goals

- Implementing NNTP, yEnc, PAR2 or RAR logic (delegated to the backend).
- Self-service registration, password reset by email, admin dashboards, billing.
- Searching indexers or integrating with Sonarr/Radarr.
- Media playback or transcoding in the browser.
- Horizontal scaling across multiple instances (single instance with SQLite by design).

### 1.3 Priorities

| Priority | Item |
| --- | --- |
| Must | Users, sessions, NZB upload, job tracking, file listing, signed single-file downloads, logo, config via JSON + env |
| Should | Server-Sent Events for live status, per-user quotas, job deletion and retention |
| Could | Download all files of a job as one zip (Section 14) |
| Won't (v1) | Admin UI, OIDC/SSO, multiple simultaneous backends per instance |

## 2. Ubiquitous language

These terms are used identically in code (type and package names), API, UI copy and this spec.

| Term | Meaning |
| --- | --- |
| User | A person allowed to log in; defined in configuration, mirrored in the database. |
| Credential | A user's password hash (argon2id); never stored in plaintext. |
| Session | An authenticated browser session, identified by an opaque random token in a cookie. |
| NZB | An XML document describing Usenet articles; the input a user uploads. |
| Job | One submitted NZB and its lifecycle, owned by exactly one user. The central aggregate. |
| Job status | The lifecycle state of a job: Queued, Importing, Ready, Failed, Deleted. |
| Backend reference | The identifiers the backend uses for a job (`nzo_id`, category, job directory). Opaque to the domain. |
| Namespace | The single backend category this Mountenant instance owns (`backend.category`, default `mountenant`). Every job lives in its own directory `<namespace>/<jobId>/`. Isolation between users is enforced by Mountenant's catalogue, not by the backend (ADR 0002). |
| Job file | A file available inside a Ready job: relative path, size, content type. |
| Download link | A signed, expiring URL that grants a GET on exactly one job file (or one job archive). |
| Quota | Per-user limits: active jobs, total jobs, NZB size, bytes downloaded per period. |
| Retention | How long a job stays available before it is removed from Mountenant and the backend. |
| Backend | The configured AltMount or NzbDav instance, reached through an adapter; Mountenant's only content source. |

## 3. Architecture

Mountenant is a modular monolith: one Go process with three bounded contexts behind a thin HTTP layer, SQLite for metadata, and an adapter that is the only code allowed to talk to the backend (AltMount or NzbDav).

&#91;embedded content: system context · Mountenant, AltMount, Usenet provider\]

The browser never sees the backend; every byte of a download flows Usenet provider → backend → Mountenant → browser, with Mountenant adding authorization, signing and accounting only.

### 3.1 Key decisions

| Decision | Choice | Alternative rejected |
| --- | --- | --- |
| Deployment unit | Single binary with embedded SPA | Separate frontend hosting (CORS, two deploys) |
| Architecture style | Modular monolith, hexagonal per context | Microservices (overhead without benefit at this scale) |
| Content path | Streaming proxy, no local storage | Spool to disk (disk is the constraint) |
| Backend coupling | SABnzbd API + WebDAV standards | Product-specific internal APIs (less stable, not interchangeable) |
| Live updates | Server-Sent Events | WebSockets (bidirectional not needed) |
| Auth model | Server-side sessions + cookies | JWT in browser storage (revocation, XSS exposure) |

Architecture decisions are recorded as ADRs (Michael Nygard format) in `docs/adr/`.

## 4. Domain model

The domain splits into three bounded contexts; Jobs is the core domain, Identity and Delivery are supporting, and the backend sits behind an anti-corruption layer. Domain packages have no dependencies on HTTP, SQL or backend types (hexagonal / ports-and-adapters).

| Bounded context | Type | Responsibility | Aggregates |
| --- | --- | --- | --- |
| Identity | Supporting | Users, credentials, sessions, quotas as policy | User, Session |
| Jobs | Core | NZB intake, job lifecycle, file catalogue, retention | Job |
| Delivery | Supporting | Signed download links, streaming, byte accounting | DownloadGrant (value), UsageRecord |

### 4.1 Identity context

**User** (aggregate root)

- `UserID` (value object): UUIDv7, immutable.
- `Username` (value object): 3–32 chars, `[a-z0-9_.-]`, case-insensitive unique.
- `PasswordHash` (value object): PHC-format argon2id string.
- `Quota` (value object): `MaxActiveJobs` (jobs the backend is still working on: Queued and Importing), `MaxTotalJobs` (every job that is not Deleted, Failed included), `MaxNZBBytes`, `MaxDownloadBytesPerDay`, `MaxConcurrentDownloads`.
- `Disabled` flag; `ConfigVersion` (hash of the config entry it was last synced from).
- Invariants: username unique; a disabled user cannot authenticate and all their sessions are revoked.

**Session** (aggregate root)

- `SessionID`: SHA-256 of a 256-bit random token; the raw token exists only in the cookie.
- `UserID`, `CreatedAt`, `LastSeenAt`, `ExpiresAt` (absolute), idle timeout from config.
- Invariants: expired or revoked sessions never authenticate; rotation on login (prevents session fixation).

### 4.2 Jobs context (core)

**Job** (aggregate root)

- `JobID`: UUIDv7; also used as the public identifier.
- `OwnerID`: `UserID`; immutable.
- `NZBName`: sanitized original filename (display only).
- `NZBDigest`: SHA-256 of the decompressed NZB XML, so the same NZB uploaded plain and gzipped is one digest (deduplication per user, integrity).
- `Status`: Queued, Importing, Ready, Failed, Deleted.
- `FailureReason` (value object): code + human message; set only when Failed.
- `BackendRef` (value object): backend `nzo_id` + namespace + job directory; opaque outside the adapter.
- `Files`: list of `JobFile` value objects (`RelPath`, `Size`, `ContentType`), populated once on transition to Ready.
- `CreatedAt`, `UpdatedAt`, `ReadyAt`, `FailedAt`, `ExpiresAt`.
- Invariants:
  - Status transitions follow the state machine below; any other transition is rejected with a domain error.
  - `Files` is non-empty if and only if status is Ready.
  - `RelPath` is normalized, relative, contains no `..` segment and stays within the job's directory.
  - A job is visible to, and mutable by, its owner only.
- Domain services:
  - `SubmissionPolicy`: checks the owner's quota (active jobs, total jobs, NZB size) before a job is created.
  - `RetentionPolicy`: computes `ExpiresAt` for every job from config: Ready jobs `ReadyAt + retention`, Failed jobs `FailedAt + failedRetention`; Queued and Importing jobs are bounded by `importTimeout`, after which they become Failed and get a failed expiry. Expired jobs are deleted regardless of whether the owner is enabled or disabled.

**Job state machine**

&#91;embedded content: job lifecycle · 5 states\]

Failed is stable until deletion, Ready only as long as the backend still serves its content; Deleted is terminal and keeps the row for audit while backend content is removed.

**Additional invariants and rules**

- Every job that is not Deleted has an `ExpiresAt`; there is no state in which a job can live forever. This also covers jobs of disabled users, whose owners can no longer delete them manually.
- Ready → Failed is allowed when the backend no longer serves the job's content (code `backend_content_missing`), so Mountenant never keeps advertising files that cannot be downloaded.
- `NZBName` and `NZBDigest` are immutable after creation. A duplicate upload (same digest, same owner, existing job Queued, Importing or Ready) returns the existing job unchanged, with its original name; the new filename is not stored. Failed and Deleted jobs do not count as duplicates, so re-uploading after a failure starts a fresh job.
- `RelPath` keeps the directory structure exactly as the backend exposes it (no flattening), because flattening can create name collisions; the download filename is the base name of `RelPath`. A job is limited to `maxFilesPerJob` catalogued files.

### 4.3 Delivery context

- **DownloadGrant** (value object): `JobID`, `RelPath` (or `archive`), `UserID`, `ExpiresAt`, `Nonce`. Encoded and verified by a `Signer` port; never persisted.
- **UsageRecord** (aggregate): bytes served per user per UTC day; enforces `MaxDownloadBytesPerDay`.

* **DownloadSlots** (domain service): counts open download streams per user and enforces `MaxConcurrentDownloads`; a slot is acquired before the first byte and released when the stream ends or the client disconnects. Held in memory (single instance by design), never persisted.

### 4.4 Domain events

Events are in-process (no message broker) and published after the transaction commits.

| Event | Raised when | Consumers |
| --- | --- | --- |
| JobSubmitted | Job created and NZB accepted by the backend | SSE notifier |
| JobImportStarted | Backend reports active import | SSE notifier |
| JobBecameReady | Backend import complete, files catalogued | SSE notifier |
| JobFailed | Backend reports failure or timeout, or a Ready job's content is gone | SSE notifier, logs |
| JobDeleted | User deletion or retention expiry | Backend cleanup, SSE notifier |
| UserDisabled | Config sync disables a user | Session revocation |

### 4.5 Ports (interfaces owned by the domain)

- `JobRepository`, `UserRepository`, `SessionRepository`, `UsageRepository`: persistence.
- `Backend`: `Ping()` (readiness: API key and WebDAV credentials), `Submit(nzb, jobId)`, `Find(jobId)` (look up a job by name before a retry), `Status(ref)`, `ListFiles(ref)`, `Open(ref, relPath, byteRange)` (`byteRange` is always a closed in-bounds range or none, see 10.2), `Delete(ref)`, `VerifyGone(ref)`, `ListNamespace()` (orphan sweep). The namespace is adapter configuration, not a parameter.
- `Signer`: `Sign(grant)`, `Verify(token)`.
- `Clock` and `IDGenerator`: injected for deterministic tests.
- `PasswordHasher`: argon2id behind an interface.

## 5. Functional requirements

Each use case maps to one application-layer command or query handler; handlers orchestrate domain objects and ports and contain no business rules themselves.

### 5.1 Identity

- **UC-01 Log in.** Username + password. On success: new session, cookie set, previous session token for that browser discarded. On failure: generic error (no user enumeration), constant-time comparison, rate limited per IP and per username.
- **UC-02 Log out.** Revokes the current session server-side and clears the cookie.
- **UC-03 Get current user.** Returns username, quota limits and current usage for the UI.
- **UC-04 Sync users from config.** On startup and on SIGHUP: create missing users, update hashes and quotas, disable users removed from config (never hard-delete, so job ownership stays intact). Idempotent.

### 5.2 Jobs

- **UC-10 Submit NZB.** Multipart upload of one `.nzb` or `.nzb.gz`. Gzip is detected by its magic bytes, not only by file name, and decompressed as a stream bounded by the owner's `MaxNZBBytes` (capped by `jobs.maxNzbBytesHardCap`); the limit applies to the decompressed XML, and overflow answers 413 `nzb_too_large`. Validation: well-formed XML (XXE-safe parser, see 7.6), at least one file and one segment. The backend always receives plain XML (neither backend accepts `.nzb.gz`; ADR 0005). Quota check via `SubmissionPolicy`. Duplicate digest for the same user returns the existing job unchanged (HTTP 200, original name kept, response flag `duplicate: true`) instead of creating a new one; Failed and Deleted jobs are not considered duplicates. The same NZB from different users creates separate jobs and separate backend imports. The job is committed as Queued **before** it is submitted to the backend (the orphan sweep relies on this ordering), then submitted into the namespace as `<jobId>.nzb`.
- **UC-11 List my jobs.** Paginated (cursor-based), newest first, filter by status.
- **UC-12 Get job.** Status, timestamps, failure reason, and the file list when Ready.
- **UC-13 Delete job.** Owner only. Marks Deleted, removes it from the backend asynchronously, invalidates outstanding download links for that job. Backend removal counts as done only when `VerifyGone` confirms it (10.2, ADR 0004); until then the reconciler retries with backoff and alerts after repeated failures.
- **UC-14 Reconcile job status.** Background worker polls the backend for non-terminal jobs, applies transitions, catalogues files on completion, fails jobs exceeding the import timeout. It also re-verifies Ready jobs every readyCheckInterval (a shallow listing of the job directory, checking that every catalogued file is still listed) and moves a Ready job to Failed with backend\_content\_missing when its content is gone. Backend queue/history status is not trusted for this: AltMount keeps reporting `Completed` for files it no longer serves.
- **UC-15 Expire jobs.** Background worker deletes jobs past `ExpiresAt` (same path as UC-13).

* **UC-16 Sweep orphans.** Background worker runs every `orphanSweepInterval`: lists the job directories and history rows in the namespace (`ListNamespace`) and deletes those with no matching non-Deleted job (crash after submit, failed deletes, manual backend changes). Race safety: the backend is listed **before** job IDs are read from the database; only names of the form `<uuid>` or `<uuid> (n)` are considered (other items are left alone); entries younger than `importTimeout`, measured against the backend's own `Date` header, are skipped. Runs once ~10 minutes after startup and then every interval, never concurrently with itself. Deletes are logged; a dry-run mode is available in config for the first deployment.
* **UC-17 Purge NZB blobs.** A held NZB blob is deleted as soon as the backend accepts the job, and in any case when the job reaches Failed or Deleted; a startup check removes blobs whose job is terminal.

### 5.3 Delivery

- **UC-20 Issue download link.** For a file of a Ready job owned by the caller: returns a signed URL valid for a configurable TTL (default 15 minutes).
- **UC-21 Download file.** Anonymous endpoint verified only by signature. Streams from the backend and sets `Content-Disposition` (RFC 6266), `Content-Type`, `Content-Length`, `Accept-Ranges`, `ETag`, `Last-Modified`. Mountenant evaluates `Range` / `If-Range` itself (RFC 9110 §14, `http.ServeContent` semantics) against the catalogued size and its own validators, and forwards at most one closed in-bounds range to the backend (10.2, ADR 0003). Multi-range requests are answered with the full file (200). `ETag` is derived from job ID, `RelPath` and size; `Last-Modified` is the job's `ReadyAt`; `Content-Type` comes from the file extension. Counts bytes against the owner's daily download quota. Rejects with 429 `download_limit` when the owner already has `MaxConcurrentDownloads` streams open (each range request of a resumed download counts while it streams).
- **UC-22 Download all as zip** (optional, Section 14).

### 5.4 Live updates

- **UC-30 Subscribe to job events.** Server-Sent Events stream for the authenticated user, emitting only their own job events. The UI falls back to polling when SSE is unavailable. On reconnect with a `Last-Event-ID` older than the retained events (24 h), unknown, or absent, the server sends a single `reset` event instead of a replay; the client then refetches the job list and any open job detail. The client also refetches on every reconnect and on window focus, so a gap in events can never leave the UI in a stale state.

## 6. HTTP API

The API is JSON over HTTPS, versioned under `/api/v1`, and specified contract-first in an OpenAPI 3.1 document that is the single source of truth: Go server stubs (oapi-codegen) and the TypeScript client (openapi-typescript + openapi-fetch) are generated from it.

### 6.1 Conventions

- Resource-oriented paths, plural nouns, lowercase kebab-case.
- Errors use RFC 9457 Problem Details (`application/problem+json`) with a stable `type` URI per error and a machine-readable `code`.
- Timestamps are RFC 3339 UTC; IDs are UUIDv7 strings.
- Pagination is cursor-based: `?limit=&cursor=`, response carries `nextCursor`.
- Mutating requests require the session cookie plus a CSRF token header (Section 7).
- `POST /jobs` is idempotent through the NZB digest: repeating an upload returns the existing job (UC-10). An `Idempotency-Key` header is not needed and not stored in v1.
- Rate limit responses use 429 with `Retry-After`; `RateLimit` headers follow the IETF draft.

### 6.2 Endpoints

| Method | Path | Auth | Purpose | Success |
| --- | --- | --- | --- | --- |
| POST | `/api/v1/auth/login` | none | UC-01 log in | 204 + cookie |
| POST | `/api/v1/auth/logout` | session | UC-02 log out | 204 |
| GET | `/api/v1/me` | session | UC-03 current user, quota, usage | 200 |
| GET | `/api/v1/csrf` | session | Issue CSRF token | 200 |
| POST | `/api/v1/jobs` | session | UC-10 submit NZB (multipart, field `nzb`) | 201 (200 on duplicate) |
| GET | `/api/v1/jobs` | session | UC-11 list jobs | 200 |
| GET | `/api/v1/jobs/{jobId}` | session | UC-12 job detail with files | 200 |
| DELETE | `/api/v1/jobs/{jobId}` | session | UC-13 delete job | 202 |
| POST | `/api/v1/jobs/{jobId}/files/links` | session | UC-20 issue link (body: `path`) | 201 |
| POST | `/api/v1/jobs/{jobId}/archive/links` | session | UC-22 issue zip link (optional) | 201 |
| GET | `/api/v1/events` | session | UC-30 SSE stream | 200 `text/event-stream` |
| GET | `/dl/{token}` | signature | UC-21 / UC-22 download | 200 / 206 |
| GET | `/healthz` | none | Liveness | 200 |
| GET | `/readyz` | none | Readiness (DB + backend reachable) | 200 / 503 |
| GET | `/metrics` | config-gated | Prometheus metrics | 200 |

Ownership failures return 404, not 403, so job IDs of other users cannot be probed.

### 6.3 Representative payloads

- Job: `id`, `name`, `status`, `failure {code, message}`, `createdAt`, `readyAt`, `expiresAt`, `files [{path, size, contentType}]`.
- Link: `url`, `expiresAt`.
- Me: `username`, `quota {maxActiveJobs, maxTotalJobs, maxNzbBytes, maxDownloadBytesPerDay, maxConcurrentDownloads}`, `usage {activeJobs, totalJobs, downloadedBytesToday, activeDownloads}`.

### 6.4 SSE events

Event names mirror domain events: `job.submitted`, `job.importing`, `job.ready`, `job.failed`, `job.deleted`. Each `data` payload is the Job representation; every event carries an `id` so clients resume with `Last-Event-ID`. A heartbeat comment is sent every 25 seconds to keep proxies from closing the stream.

A `reset` event (no job payload) is sent when the requested `Last-Event-ID` can no longer be replayed; the client must refetch its state (UC-30).

### 6.5 Error codes

| HTTP | code | When |
| --- | --- | --- |
| 400 | `invalid_nzb` | Unparseable or empty NZB |
| 401 | `unauthenticated` | No or invalid session |
| 403 | `csrf_failed` | Missing or wrong CSRF token |
| 403 | `link_invalid` | Bad signature or expired download link |
| 404 | `not_found` | Unknown or foreign resource |
| 409 | `job_not_ready` | Link requested for a non-Ready job |
| 413 | `nzb_too_large` | Upload exceeds quota or hard cap |
| 422 | `quota_exceeded` | Active or total job limit reached |
| 429 | `rate_limited` | Login or API rate limit |
| 502 | `backend_unavailable` | Backend unreachable or erroring |
| 429 | download\_limit | Concurrent download limit reached on /dl |
| 410 | content\_missing | File of a Ready job is gone on the backend; the job is moved to Failed |

## 7. Security

Security follows the OWASP ASVS 4.0 Level 2 baseline and the OWASP cheat sheets for sessions, CSRF, password storage and file upload; the backend is never exposed to users.

### 7.1 Authentication and passwords

- Passwords hashed with argon2id (RFC 9106 recommended parameters: m=64 MiB, t=3, p=4), stored in PHC string format.
- Config files contain hashes only; a `mountenant hash-password` CLI subcommand produces them. Plaintext passwords in config are rejected at startup.
- Login rate limit: token bucket per IP and per username; exponential backoff after repeated failures.
- Failed and successful logins are written to the security event log (7.8), built for fail2ban; passwords and tokens never appear in any log.

* Hashing is bounded to protect memory on small hosts: the rate limiter runs before any hashing, and at most `auth.maxConcurrentHashes` (default 2) argon2id computations run at once behind a semaphore. Excess login requests wait at most 2 s, then get 429 `rate_limited`. Worst-case hashing memory is therefore `maxConcurrentHashes × memory cost` (128 MiB by default), and the argon2 parameters are configurable for hosts with less RAM.
* Unknown usernames are verified against a fixed dummy hash under the same semaphore, so response time does not reveal whether a user exists.

### 7.2 Sessions

- Server-side sessions in SQLite; the cookie holds a 256-bit random token, the database stores its SHA-256.
- Cookie: `__Host-mountenant_session`, `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, no `Domain`.
- Idle timeout (default 12 h) and absolute lifetime (default 7 d), both configurable.
- Session ID rotated on login; all sessions revoked when a user is disabled or their password hash changes.

### 7.3 CSRF

- Synchronizer token pattern: `GET /api/v1/csrf` returns a token bound to the session; all unsafe methods require it in `X-CSRF-Token`.
- Additionally, unsafe requests must carry an `Origin` header matching the configured public URL (Fetch Metadata / `Sec-Fetch-Site` checked when present).

### 7.4 Signed download URLs

- Format: `/dl/{token}` where token = base64url(payload) + `.` + base64url(HMAC-SHA256(key, payload)).
- Payload (compact, versioned): `v`, `kid` (key id), `jid` (job), `p` (relative path or archive marker), `uid` (owner), `exp` (Unix seconds), `n` (random nonce).
- Verification: constant-time MAC check, `exp` in the future (≤ 60 s clock skew), job exists, owned by `uid`, status Ready, path in the catalogued file list.
- Key rotation: config holds a list of keys with ids; the first signs, all verify. Minimum key length 32 bytes, supplied via env or secret file.
- TTL default 15 minutes, max 24 hours. The link stays usable for resumed range requests until it expires.
- Deleting a job revokes its links implicitly, because verification re-checks job state.
- Responses for `/dl` set `Cache-Control: private, no-store` and `Referrer-Policy: no-referrer`; tokens are redacted in access logs.

### 7.5 Tenant isolation

- All jobs of all users share one backend category (the namespace, ADR 0002); each job has its own directory named by its job ID. All backend paths are built server-side from `namespace + jobId + catalogued relPath`, with each segment URL-escaped. Client-supplied paths are only matched against the catalogue, never concatenated. Ownership is checked on every lookup, so sharing the backend category gives users no access to each other's jobs.
- Every repository query for jobs is scoped by `owner_id`; there is no unscoped job query in the user-facing code path.
- The backend listens on a private network or loopback only; its credentials live in Mountenant's config and are never sent to the browser.

### 7.6 Upload hardening

- Hard cap on request size before parsing (`http.MaxBytesReader`), lower per-user cap via quota.
- `.nzb.gz` decompressed as a stream with a size limit equal to the NZB quota (zip-bomb guard); memory never exceeds that limit regardless of compression ratio.
- XML parsed with Go's `encoding/xml`, which does not resolve entities. The standard NZB declaration `<!DOCTYPE nzb PUBLIC …>` is accepted, because real-world NZBs carry it; DOCTYPEs with an internal subset (`[`) and any `<!ENTITY` declaration are rejected (ADR 0005).
- Uploaded filename used for display only, sanitized; never used to build a path.

### 7.7 HTTP hardening

- Mountenant always runs behind a reverse proxy (Caddy, nginx or Traefik), which terminates TLS and sets HSTS. Mountenant serves plain HTTP on loopback or a private interface only, has no in-process TLS, and refuses to start unless trusted proxies are configured and the public URL uses https.
- Headers: strict `Content-Security-Policy` (`default-src 'self'`, no inline scripts), `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, `Permissions-Policy` minimal, `frame-ancestors 'none'`.
- Server timeouts: `ReadHeaderTimeout`, `IdleTimeout` set; `WriteTimeout` disabled only on `/dl` and SSE routes, which instead use per-write deadlines.
- The client IP is the rightmost untrusted address in `X-Forwarded-For`, read only when the direct peer is in `server.trustedProxies`; requests whose direct peer is not a trusted proxy are rejected with 403, so the app cannot be reached around the proxy.
- Dependencies scanned with `govulncheck` and `npm audit` in CI.

### 7.8 Security event log (fail2ban)

Authentication and abuse events go to a dedicated security log with one fixed, documented line format, so fail2ban (or CrowdSec) can match them with a stable regex independent of the application log level or format.

- Destination: a file (`logging.securityLog.path`, default `/var/log/mountenant/security.log`) and, optionally, stdout for journald; the file is opened in append mode and reopened on SIGHUP for logrotate.
- Format: one line per event, plain text, space-separated `key=value`, values without spaces or quoted with `"` escaped; usernames are sanitised (control characters stripped, length capped) to prevent log injection.
- Line layout: `<RFC 3339 UTC timestamp> mountenant event=<name> ip=<client ip> user="<username>" reason=<code>`.
- Events: `auth_failure` (unknown user or wrong password, same line for both), `auth_success`, `auth_rate_limited`, `csrf_failure`, `link_invalid` (tampered or expired signed URL), `forbidden_peer` (request not from a trusted proxy).
- The IP is always the real client IP resolved through the trusted proxy chain, never the proxy's address; a wrong IP would ban the proxy.
- `deploy/fail2ban/` ships a filter (`mountenant.conf`) and a jail example (`maxretry`, `findtime`, `bantime`) matching `auth_failure` and `link_invalid`; a test asserts the filter's regex matches the emitted lines (`fail2ban-regex` in CI).
- The format is part of the public contract: changes need a version note in the changelog.

## 8. Configuration

Configuration follows the 12-factor model: JSON files carry structure, environment variables override any scalar and carry secrets, and the process fails fast on any invalid value. Both files are validated against published JSON Schemas (draft 2020-12) shipped in the repo, so editors can autocomplete and CI can lint them.

### 8.1 Sources and precedence

1. Built-in defaults.
2. `config.json` (path from `MOUNTENANT_CONFIG`, default `/etc/mountenant/config.json`).
3. `users.json` (path from `MOUNTENANT_USERS`, default next to `config.json`).
4. Environment variables `MOUNTENANT_<SECTION>_<KEY>` (e.g. `MOUNTENANT_SERVER_PUBLIC_URL`).
5. Secret files: any key with a `_FILE` suffix reads its value from a file (Docker/Kubernetes secrets convention), e.g. `MOUNTENANT_SIGNING_KEYS_FILE`.

Unknown keys are an error, not a warning. Effective config (with secrets redacted) is logged at startup.

### 8.2 `config.json` sections

| Section | Keys |
| --- | --- |
| `server` | `listenAddr` (loopback or private address), `publicUrl` (must be https), `trustedProxies[]` (required, CIDRs) |
| `database` | `path`, `busyTimeoutMs` |
| `session` | `idleTimeout`, `absoluteTimeout` (Go duration strings) |
| `signing` | `keys [{id, secret}]` (secrets normally via env/file), `defaultTtl`, `maxTtl` |
| `backend` | `type` (`altmount` or `nzbdav`), `apiUrl`, `apiKey`, `webdavUrl`, `webdavUser`, `webdavPassword`, `category` (default `mountenant`; must exist in the backend's config for AltMount), `requestTimeout`, `pollInterval`, `importTimeout` (default `30m`; covers backend queue wait plus import) |
| `jobs` | `retention` (default `168h`, 7 days), `maxNzbBytesHardCap`, `failedRetention` (default `24h`), `readyCheckInterval` (default `1h`), `orphanSweepInterval` (default `6h`), `orphanSweepDryRun`, `maxFilesPerJob` (default 10000) |
| `defaults.quota` | `maxActiveJobs`, `maxTotalJobs`, `maxNzbBytes`, `maxDownloadBytesPerDay`, `maxConcurrentDownloads` |
| `rateLimits` | `login {perMinute, burst}`, `api {perMinute, burst}` |
| `logging` | `level`, `format` (`json` or `text`), `securityLog {path, stdout}` |
| `metrics` | `enabled`, `listenAddr` (separate port, not public) |
| auth | maxConcurrentHashes (default 2), hashWaitTimeout (default 2s), argon2 {memoryKiB, iterations, parallelism} |

### 8.3 `users.json`

- Array of users: `username`, `passwordHash` (argon2id PHC string), optional `quota` override, optional `disabled`.
- Read at startup and on SIGHUP (UC-04); a file watcher is not used, to keep behaviour explicit.
- Removing a user from the file disables them; it never deletes their jobs.
- The file should be mode 0600 and owned by the service user; the process warns if it is world-readable.

### 8.4 CLI

Single binary with subcommands (standard library `flag` or a minimal CLI library):

- `mountenant serve`: run the server.
- `mountenant hash-password`: read a password from stdin (no echo), print the PHC hash.
- `mountenant config validate`: validate config and users files, exit non-zero on error.
- `mountenant migrate`: apply database migrations (also run automatically by `serve`).

## 9. Persistence

SQLite holds only metadata (users, sessions, jobs, file catalogue, usage); content bytes never touch it. Access goes through repository adapters implementing the domain ports, with SQL written by hand and type-checked via sqlc.

### 9.1 Engine and driver

- Driver: `modernc.org/sqlite` (pure Go, keeps `CGO_ENABLED=0` and a static binary). Alternative if profiling demands: `mattn/go-sqlite3` with CGO.
- Pragmas at connection open: `journal_mode=WAL`, `synchronous=NORMAL`, `foreign_keys=ON`, `busy_timeout` from config.
- Connection pools: one writer connection (`SetMaxOpenConns(1)`) and a separate read pool, the established pattern for SQLite under WAL to avoid `SQLITE_BUSY`.
- Transactions use `BEGIN IMMEDIATE` for writes.

### 9.2 Migrations

- Versioned, forward-only SQL migrations embedded in the binary (`go:embed`), applied with `pressly/goose` (or `golang-migrate`).
- Applied automatically at startup inside a transaction; startup aborts on failure.
- Online backup via `VACUUM INTO` or the SQLite backup API, documented for operators; Litestream is an option for continuous replication.

### 9.3 Schema

| Table | Columns (key ones) | Notes |
| --- | --- | --- |
| `users` | `id` PK, `username` UNIQUE COLLATE NOCASE, `password_hash`, `quota_json`, `disabled`, `config_version`, `created_at`, `updated_at` | Mirrors `users.json` |
| `sessions` | `id` PK (token hash), `user_id` FK, `csrf_secret`, `created_at`, `last_seen_at`, `expires_at` | Index on `user_id`, `expires_at` |
| `jobs` | `id` PK, `owner_id` FK, `nzb_name`, `nzb_digest`, `status`, `failure_code`, `failure_message`, `backend_ref_json`, `created_at`, `updated_at`, `ready_at`, `expires_at` | UNIQUE(`owner_id`, `nzb_digest`) where status not in (Failed, Deleted); index on (`owner_id`, `created_at` DESC) and (`status`) |
| `job_files` | `job_id` FK ON DELETE CASCADE, `rel_path`, `size`, `content_type` | PK (`job_id`, `rel_path`) |
| `job_events` | `id` INTEGER PK, `job_id`, `owner_id`, `type`, `payload_json`, `created_at` | Feeds SSE replay via `Last-Event-ID`; pruned after 24 h |
| `usage_daily` | `user_id`, `day`, `bytes_served` | PK (`user_id`, `day`) |
| `nzb_blobs` | `job_id` PK, `content` BLOB | Optional; keeps the NZB until the backend confirms acceptance, enabling retries; deleted on acceptance, or when the job turns Failed or Deleted (UC-17) |

`jobs` also carries the reconciler's bookkeeping (migration 00002): `next_check_at` (when the job is next due: status poll, ready re-check, submit retry or backend cleanup), `attempts` (consecutive failed backend calls, for backoff and alerting), `backend_removed_at` (when `VerifyGone` confirmed the cleanup of a Deleted job) and `version` (optimistic concurrency between user actions and the reconciler).

- Times are stored as Unix milliseconds (ADR 0007).
- `status` stored as text with a CHECK constraint listing allowed values.
- Domain events are written to `job_events` in the same transaction as the state change (transactional outbox), then dispatched; this keeps SSE consistent with the database.

## 10. Backend integration

The backend is interchangeable: AltMount and NzbDav are both supported in v1, selected by `backend.type` in config, and further backends can be added as new adapters without touching the domain. Both products expose the same two standard interfaces (a SABnzbd-compatible API for intake and status, WebDAV for listing and byte access), so one generic adapter covers the shared protocol and a per-product profile covers the differences. The phase-2 case study (`docs/feasibility.md`) verified every operation below against live instances; the profiles differ more than intake/status strings, so each profile also owns delete semantics, URL layout and known quirks.

### 10.1 Adapter structure

- `Backend` port (Section 4.5) is the only contract the domain knows.
- `sabdav` adapter: generic implementation over the SABnzbd API + WebDAV, parameterised by a `Profile`.
- `Profile` per product (`altmount`, `nzbdav`): API path, WebDAV job-directory layout, status mapping, error style, delete procedure, href parsing, readiness probe, known quirks. Profiles are data plus small functions, not forks of the adapter.
- A product that does not fit the shared protocol gets its own adapter implementing `Backend`; nothing else changes.
- Exactly one backend is active per Mountenant instance; switching backends is a configuration change on a fresh instance (existing jobs are not migrated).

### 10.2 Operations

| Port method | Interface | Notes |
| --- | --- | --- |
| `Submit` | SABnzbd API `mode=addfile`, `cat=<namespace>`, `nzbname=<jobId>`, multipart field `name` with filename `<jobId>.nzb`, plain XML only | Both products name the job directory after the multipart filename, so the directory is `<namespace>/<jobId>/`. AltMount uses `nzbname` as `nzo_id`; NzbDav ignores it and returns a random GUID. `BackendRef` stores the returned `nzo_ids[0]`. |
| `Find` | `mode=queue` + `mode=history` (category filter), match `name == jobId` | Called before every re-submission (10.4): re-submitting a known job duplicates it (NzbDav `<jobId> (2)`) or merges directories (AltMount). |
| `Status` | `mode=queue&nzo_ids=` first, then `mode=history&nzo_ids=` | Lookups by `nzo_ids` bypass AltMount's 7-day history window. Mapped by the profile (10.3). Always check the JSON `status` field: AltMount reports API errors as HTTP 200 with `status:false`. |
| `ListFiles` | WebDAV `PROPFIND Depth: infinity` on the job directory | Use only the URL path of each `href` (NzbDav returns absolute URLs with an internal host), percent-decode, detect directories by `resourcetype`. Size from `getcontentlength`; backend `getcontenttype` and `getetag` are ignored (10.6). |
| `Open` | WebDAV `GET`, with at most `Range: bytes=a-b` where `0 ≤ a ≤ b ≤ size-1` | Mountenant never forwards suffix, open-ended, multi-range, `If-Range`, `If-None-Match` or `If-Modified-Since` (ADR 0003). Response validated: 206 with the expected `Content-Range`, or 200 without range, and the expected `Content-Length`. Only allowlisted headers are copied to the client. |
| `Delete` | Per profile, see table below | Success is never inferred from a response code (ADR 0004). |
| `VerifyGone` | `mode=queue&nzo_ids=` and `mode=history&nzo_ids=` return no slot, and `PROPFIND Depth: 0` on the job directory returns 404 | The authoritative "deleted" signal on both products. |
| `ListNamespace` | WebDAV `PROPFIND Depth: 1` on the namespace directory, plus SABnzbd history filtered by category | Used only by the orphan sweep (UC-16). |

Profile specifics (pinned versions, 10.5):

| | AltMount 0.3.2 | NzbDav 0.6.4 |
| --- | --- | --- |
| SAB API | `/sabnzbd/api` | `/api` |
| Job directory (WebDAV) | `/webdav/complete/<namespace>/<jobId>/` | `/content/<namespace>/<jobId>/` |
| Category must pre-exist | yes (`invalid category`) | no |
| Delete | `mode=history&name=delete&value=<nzo>` (removes the row only, always `status:true`) **and** WebDAV `DELETE` on the job directory (204, idempotent) | `mode=history&name=delete&value=<nzo>&del_completed_files=1` (the standard `del_files` is ignored; with this parameter an unknown id answers 200, without it 500); WebDAV `DELETE` on the job directory as fallback when the row is gone but the directory remains |
| API error style | HTTP 200 + `status:false` | 401 / 500 + `status:false` |
| Readiness probe | `mode=version` with key + `PROPFIND Depth: 0` on the namespace | `mode=queue` with key (`mode=version` needs no key) + `PROPFIND Depth: 0` on the namespace (404 until the first import) |
| Range quirks | suffix ranges and range ends past EOF permanently hide the file (fixed upstream only partly, ADR 0003) | suffix ranges return wrong bytes; `If-Range` ignored |

Backend URLs are built by escaping each path segment; `RelPath`s come from releases and may contain spaces, `#`, `%`, `?` or non-ASCII characters.

### 10.3 Status mapping

The queue is checked before the history. Observed and source-confirmed values:

| Backend value | Where | Mountenant status |
| --- | --- | --- |
| `Queued`, `Paused` | queue | Queued (the UI shows "waiting for backend") |
| `Downloading` | queue | Importing (both products only build metadata; no content is downloaded) |
| `Unknown` | AltMount history | Ignored; the item is still in the queue |
| `Completed` | history | Importing → Ready once `ListFiles` returns ≥ 1 file |
| `Failed` | history | Failed `backend_failed`; `fail_message` becomes the failure message (it is clear enough to show users) |
| No progress within `importTimeout` | — | Failed `import_timeout` |

Unknown backend states are logged and leave the job unchanged (no guessing).

Ready is not assumed permanent. A Ready job is re-verified every `readyCheckInterval` (UC-14), and a 404 from WebDAV on the job directory, a catalogued file missing from it, or a 404 on a file during `Open` moves the job to Failed with `backend_content_missing`; the download in progress answers 410 `content_missing` and SSE emits `job.failed`. Recovery is a fresh upload, which creates a new job and a new job directory; the old directory is never reused.

### 10.4 Resilience

- All calls carry a context timeout; idempotent reads retry with exponential backoff and jitter (max 3).
- Submission is retried by the reconciler if the NZB is still held in `nzb_blobs` and the backend was unavailable. Before every retry the reconciler calls `Find(jobId)`; if the backend already knows the job, it records that `nzo_id` instead of submitting again.
- `Open` errors before the first byte (5xx, failed response validation) are retried, then answered 502 `backend_unavailable`; a 404 answers 410 `content_missing`. The upstream status code is never passed through.
- If the backend body ends or breaks after the first byte, the proxy reopens the backend for exactly the remaining bytes and continues the same response, up to `MaxResumes` (default 3) times (ADR 0006). AltMount 0.3.2 cuts full downloads short intermittently with a 200 and a full `Content-Length`; the spike resumed 3/3 such cut-offs byte-perfectly. Only when resumes are exhausted is the client connection aborted (`http.ErrAbortHandler`) so the client sees a short body; a backend error never becomes a complete-looking file.
- Deletion is retried by the reconciler until `VerifyGone` succeeds; jobs whose backend cleanup keeps failing are logged and exported as a metric for alerting. The orphan sweep (UC-16) is the final safety net.
- A circuit breaker opens after repeated failures and makes `/readyz` report 503; user requests then fail fast with `backend_unavailable`.
- Client disconnects cancel the upstream WebDAV request immediately (context propagation), so no provider bandwidth is wasted (verified on both backends).
- Mutations (addfile, queue and history delete) are never retried automatically.

### 10.5 Assumptions about the backend

- One shared Usenet provider account; the backend enforces connection limits, Mountenant enforces per-user fairness via quotas and concurrent download limits.
- Any local cache the backend keeps is size-capped in its own config to fit available disk.
- The backend is reachable only on a private network; its UI, API key and WebDAV credentials are known only to Mountenant.
- Pinned versions: AltMount 0.3.2 (`14ad0f5`) and NzbDav 0.6.4. A new version is adopted only after the contract tests and the range conformance matrix (`docs/feasibility.md` §4.4) pass against it.
- Content limits of both products, surfaced to users through the job's failure message: compressed RAR archives cannot be served (only store-mode RARs and plain files), and imports fail when articles are missing.
- Required backend configuration, documented for operators (it cannot be verified through the SAB API or WebDAV, see `docs/feasibility.md` §3):

| Product | Setting | Value | Why |
| --- | --- | --- | --- |
| AltMount | `sabnzbd.enabled` | `true` | SAB API |
| AltMount | `sabnzbd.categories` | contains `backend.category` | unknown categories are rejected |
| AltMount | `import.allowed_file_extensions` | `[]` (all) | default allows only media files |
| AltMount | `import.expand_bluray_iso` | `false` (config file / API only) | default replaces an `.iso` with the largest file inside it |
| AltMount | `import.rename_to_nzb_name` | `false` (config file / API only) | default renames files to the job ID |
| NzbDav | `api.ensure-importable-video` | `false` | default fails every non-video NZB |
| NzbDav | `webdav.enforce-readonly` | `false` | WebDAV `DELETE` is needed for the delete fallback and the orphan sweep |

### 10.6 Validators and content type

Mountenant owns `ETag` (derived from job ID, `RelPath` and size), `Last-Modified` (`ReadyAt`) and `Content-Type` (from the file extension). Backend values are not used: NzbDav sends no `ETag` and always `application/octet-stream`, AltMount sends its `ETag` only in `PROPFIND`, and both backends' clocks were observed about two hours off. Backend timestamps are never compared with Mountenant's clock, except relative to the backend's own `Date` header (UC-16).

## 11. Frontend

The UI is a React + TypeScript single-page app built with Vite, embedded in the Go binary and served from the same origin as the API. It uses a small set of mainstream libraries and no custom state framework.

### 11.1 Stack

| Concern | Choice | Reason |
| --- | --- | --- |
| Language | TypeScript, `strict: true` | Type safety end to end with the generated client |
| Build | Vite | Standard React tooling, fast dev server with `/api` proxy |
| Routing | React Router | De facto standard |
| Server state | TanStack Query | Caching, retries, invalidation; SSE events invalidate queries |
| API client | openapi-typescript + openapi-fetch | Generated from the OpenAPI spec, no hand-written types |
| Forms | React Hook Form + Zod | Validation shared in schema form |
| UI components | Radix UI primitives (or shadcn/ui) + Tailwind CSS | Accessible primitives, no heavy design system |
| Upload | `XMLHttpRequest` with progress events | Native upload progress |
| Tests | Vitest + React Testing Library; Playwright for end-to-end | Standard pairing |
| Lint/format | ESLint (typescript-eslint) + Prettier | Standard |

### 11.2 Screens

- **Login**: logo, username, password, generic error message, disabled button while submitting.
- **Jobs** (home): drop zone + file picker for `.nzb`/`.nzb.gz` (multiple files queued client-side, uploaded one request each), upload progress, list of jobs with status badge, name, created time, expiry; filter by status; live updates via SSE.
- **Job detail**: status timeline, failure reason, file table (path, size), per-file Download button, optional "Download all" button, Delete with confirmation.
- **Account** (menu): username, quota usage bars, Log out.
- **Not found / error**: friendly page with logo and link home.

### 11.3 Interaction rules

- Download buttons request a link (UC-20) and then navigate the browser to it (`window.location` / anchor), so the browser's own download manager handles saving and resume; bytes never pass through JavaScript.
- Optimistic UI only for delete; everything else reflects server state.
- 401 from any call redirects to Login and preserves the intended route.
- Problem Details `code` values map to translated, user-facing messages in one place.

* A duplicate upload shows a notice naming the existing job by its original name and links to it, instead of adding a new row.
* Nested files are shown with their full relative path, grouped by folder, with the base name emphasised; the browser saves each file under its base name.
* After an SSE `reset`, a reconnect, or window focus, the job list and open job detail are refetched; the UI never derives state only from the event stream.

### 11.4 Logo and brand

- The logo is supplied externally; Mountenant only defines the requirements and the generation prompt below. Until it arrives, a plain wordmark placeholder is used.
- Deliverables expected: master SVG (single colour + one accent), monochrome variant for dark mode, 32 px favicon/ICO, 180 px apple-touch-icon, 512 px PWA icon. Raster outputs from an image generator are vectorised before use.
- Placement: header left (logo + wordmark, links home), login card centre, favicon, browser tab title `Mountenant`.
- Wordmark font: a geometric sans (e.g. Inter or Manrope), self-hosted because the CSP forbids external font CDNs.
- Colour tokens defined once as CSS custom properties with light and dark values; respects `prefers-color-scheme`.

**Logo generation prompt**

```
Design a minimalist flat vector logo for "Mountenant", a self-hosted web app that lets several users securely download files from a shared private server.

Symbol: a simple geometric mountain with two peaks, whose base doubles as a horizontal drive bay or mounting bracket. Along the base, three small evenly spaced doorways cut out as negative space, representing separate tenants sharing one system. Clean lines, consistent stroke weight, no gradients, no shadows, no 3D, no text inside the symbol.

Wordmark: "mountenant" in lowercase, geometric sans-serif similar to Inter or Manrope, medium weight, slightly increased letter spacing, placed to the right of the symbol.

Colour: deep slate (#1F2937) for the symbol and wordmark, one accent colour, teal (#14B8A6), used only on the middle doorway. Also produce a single-colour white version for dark backgrounds.

Composition: centred on a plain white background, generous padding, symbol readable at 16 px. Style: modern, calm, technical, trustworthy; comparable to logos of developer tools. Output: square symbol-only version and a horizontal symbol + wordmark lockup.
```

### 11.5 Quality bars

- WCAG 2.2 AA: keyboard operable, visible focus, labelled controls, status changes announced via `aria-live`.
- Responsive from 360 px width upward.
- English only; no i18n framework. All UI strings live in one module so copy stays consistent.
- Bundle budget: initial JS ≤ 200 KB gzipped; route-level code splitting for the job detail screen.

## 12. Non-functional requirements

Targets assume a single small VPS (2 vCPU, 2–4 GB RAM) serving tens of users; download throughput is bounded by the backend and the Usenet provider, not by Mountenant.

### 12.1 Performance

| Metric | Target |
| --- | --- |
| API p95 latency (non-download) | < 100 ms excluding backend calls |
| Memory per active download | < 1 MB (fixed-size copy buffer, `io.CopyBuffer` 64–256 KB) |
| Concurrent downloads | ≥ 50 without degradation of API latency |
| Mountenant overhead on throughput | < 5 % vs. direct WebDAV access |
| Cold start | < 1 s including migrations on an empty DB |

### 12.2 Reliability

- Graceful shutdown on SIGTERM: stop accepting, drain API requests (default 30 s), close SSE streams, let downloads finish or cut them at the deadline.
- Background workers are restart-safe: all state lives in SQLite; reconciler is idempotent.
- No data loss on crash beyond the last uncommitted transaction (WAL + `synchronous=NORMAL`).

### 12.3 Observability

- Structured logs with `log/slog`, JSON in production; every request has a request ID (`X-Request-ID` honoured or generated), propagated to backend calls.
- Prometheus metrics: HTTP request count/latency by route and status, active downloads, bytes served, jobs by status, backend call latency and errors, backend retries and upstream resumes, circuit breaker state.
- Optional OpenTelemetry tracing behind config; off by default to stay lean.
- `/healthz` (process alive) and `/readyz` (DB writable, backend reachable) for orchestrators.

### 12.4 Operations

- Distributed as a container image (distroless, non-root, read-only root filesystem) and as a static binary.
- Data directory holds only the SQLite files; documented backup procedure.
- Reference `docker-compose.yml` with Mountenant, a backend (AltMount or NzbDav) and the mandatory reverse proxy (Caddy or nginx with buffering disabled for `/dl` and `/api/v1/events`), plus a sample fail2ban filter and jail.
- Semantic versioning; changelog per Keep a Changelog.

* The container sets `GOMEMLIMIT` below its memory limit (e.g. 80 %) so the Go runtime collects garbage before the kernel's OOM killer acts; together with the hashing semaphore (7.1) this bounds memory under login bursts.

## 13. Repository and engineering standards

One monorepo holds the Go module, the frontend and the OpenAPI contract; layering follows hexagonal architecture with dependencies pointing inward to the domain.

### 13.1 Layout

```
mountenant/
├── api/openapi.yaml            # contract, source of truth
├── cmd/mountenant/             # main: wiring only (composition root)
├── internal/
│   ├── identity/               # bounded context
│   │   ├── domain/             # User, Session, value objects, ports
│   │   ├── app/                # command/query handlers
│   │   └── adapters/           # sqlite repo, argon2 hasher
│   ├── jobs/
│   │   ├── domain/             # Job aggregate, policies, events, ports
│   │   ├── app/
│   │   └── adapters/           # sqlite repo, backend adapter (sabdav + profiles)
│   ├── delivery/
│   │   ├── domain/             # DownloadGrant, UsageRecord, Signer port
│   │   ├── app/
│   │   └── adapters/           # hmac signer, streaming proxy
│   ├── platform/               # config, db, logging, metrics, clock, events bus
│   └── http/                   # generated server, handlers, middleware, SPA embed
├── migrations/                 # SQL migrations (embedded)
├── web/                        # React + TypeScript app (Vite)
├── configs/                    # example config.json, users.json, JSON Schemas
├── deploy/                     # Dockerfile, docker-compose.yml, reverse proxy samples
└── Makefile
```

### 13.2 Rules

- `domain` packages import only the standard library and their own context's domain; a lint rule (`depguard` or `go-arch-lint`) enforces it.
- Contexts talk through application-layer interfaces or domain events, never through each other's repositories.
- HTTP handlers are thin: decode → call one app handler → encode; no business logic.
- Standard library first: `net/http` with the Go 1.22+ pattern router; add a dependency only when it replaces meaningful code (sqlc, goose, oapi-codegen, argon2 from `x/crypto`, a WebDAV client).
- Errors: wrapped with `%w`, domain errors as typed sentinels, mapped to Problem Details in one place.
- Context propagation everywhere; no global state except the composition root.

### 13.3 Code generation

- Go server interfaces and models from `api/openapi.yaml` via oapi-codegen (strict server mode).
- TypeScript types from the same file via openapi-typescript.
- SQL → Go via sqlc.
- Generated code is committed and CI fails if regeneration produces a diff.

### 13.4 Testing

| Layer | Approach |
| --- | --- |
| Domain | Pure unit tests, table-driven, no mocks needed |
| App handlers | Unit tests with in-memory fakes of ports |
| Adapters (SQLite) | Integration tests against a temp-file database with real migrations |
| Adapters (backend) | Contract tests against an `httptest` fake implementing SABnzbd API + WebDAV; one fake per profile, reproducing the observed quirks (error styles, href forms, delete semantics); the fakes assert that only closed in-bounds ranges arrive. Nightly runs against the pinned real AltMount and NzbDav containers, including the range conformance matrix and the misconfiguration `fail_message`s |
| HTTP | Handler tests via `httptest`, verifying OpenAPI conformance |
| End to end | Playwright against the built binary with the fake backend |
| Security | Tests for path traversal, cross-user access (404), expired/tampered links, CSRF, NZB DOCTYPE/entity handling and gzip bombs |

Coverage target is behavioural, not a percentage: every use case and every invariant has at least one test.

### 13.5 Tooling and CI

- Go: `gofmt`, `go vet`, `golangci-lint` (curated set), `govulncheck`, race detector in tests.
- Frontend: ESLint, Prettier, `tsc --noEmit`, Vitest, `npm audit`.
- Conventional Commits; GitHub Actions pipeline: lint → test → build frontend → build binary → build image → scan image (Trivy).
- Reproducible builds: pinned toolchain (`go.mod` toolchain directive, `.nvmrc`), `npm ci`, multi-stage Dockerfile.

## 14. Optional: download all as zip

Recommendation: build it in v1.1 as a non-resumable streamed zip, behind a feature flag; it is robust enough because file sizes are known up front, but it cannot support range requests, so large jobs should still be downloaded file by file.

### 14.1 Design

- Endpoint: a signed link with archive marker (`POST /api/v1/jobs/{id}/archive/links` → `GET /dl/{token}`).
- Format: ZIP with method Store (no compression; payloads are already compressed), ZIP64 extensions always on, UTF-8 filename flag set, per APPNOTE 6.3.x.
- Writer: Go standard library `archive/zip` with `CreateRaw`/`CreateHeader`; files streamed sequentially from backend WebDAV, one at a time, CRC-32 computed on the fly.
- `Content-Length`: computed exactly before streaming from catalogued sizes and header lengths, so browsers show progress. This requires CRC values in data descriptors rather than local headers, which `archive/zip` supports.
- No `Accept-Ranges`; a broken download restarts from zero. The UI states this next to the button.
- Limits: feature flag in config, maximum archive size and maximum file count per archive; counts against download quota like single files.

### 14.2 Why it is not fragile here

- Sizes and paths are known from the catalogue before the first byte is sent.
- A mid-stream upstream failure aborts the HTTP connection without completing the response (the handler panics with `http.ErrAbortHandler`, so Go resets the connection instead of finishing it). Because the response carries an exact `Content-Length`, browsers detect the short body and mark the download failed; and because the zip central directory is written last and never reached, even a saved partial file is an invalid archive, not a corrupt-but-complete-looking one. The failure is logged with the job and file that broke.
- Memory stays constant: one copy buffer plus zip bookkeeping.

### 14.3 What would make it fragile (explicitly out of scope)

- Resumable zips (would require deterministic byte-offset mapping and range support across entries).
- Compression (unknown output size, no `Content-Length`).
- Extracting archives server-side (the backend already exposes inner files).

## 15. Risks and decisions

The largest risk is the backend itself: AltMount and NzbDav are young, fast-moving projects, so their APIs can change and each profile must be pinned and contract-tested.

### 15.1 Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Backend API or behaviour changes between versions | Broken intake or status mapping | Pin versions; per-profile contract tests; rely on SABnzbd API + WebDAV only |
| Backend range handling destroys or corrupts content (AltMount 0.3.2 hides a file after a suffix or past-EOF range; NzbDav serves wrong bytes for suffix ranges) | Files lost or wrong bytes delivered | Mountenant evaluates ranges itself and forwards only closed in-bounds ranges (ADR 0003); upstream fixes tracked (AltMount PR #937 merged unreleased, #976 open) |
| Content the backends cannot mount (compressed RAR) | Jobs fail for some releases | Clear failure message from the backend shown to the user; documented limitation |
| Backend ends downloads early (AltMount 0.3.2, intermittent) | Failed downloads for users | Transparent upstream resume (ADR 0006); resume rate exported as a metric |
| Backend misconfiguration (extension filter, ISO expansion, video-only check) | Jobs fail or expose the wrong file | Operator checklist (10.5); nightly contract tests assert the expected `fail_message`s |
| Shared backend category: one user's job directory is reachable by path | Cross-user access | All paths built server-side from the owner-scoped catalogue; backend never exposed (ADR 0002) |
| Shared provider connections saturated by one user | Slow downloads for others | Per-user active-job and daily byte quotas; per-user concurrent download limit |
| Articles missing on the provider | Jobs fail or files partly unreadable | Surface backend health status as job failure with clear reason |
| Signed link shared by the user with third parties | Unintended access until expiry | Short TTL, downloads counted against owner quota, links die when the job is deleted |
| SQLite write contention | Latency spikes | Single writer connection, short transactions, WAL |
| Legal exposure of operating a multi-user service | Liability for content | Restrict to known users, terms of use, comply with provider terms |

### 15.2 Decided

| Topic | Decision |
| --- | --- |
| Backend | Interchangeable via adapter + profiles; AltMount and NzbDav supported in v1 |
| Concurrent downloads | Per-user `maxConcurrentDownloads` limit in v1 |
| TLS | Always behind a reverse proxy; no in-process TLS |
| Failed logins | Dedicated security log in a fixed format for fail2ban |
| UI language | English only |
| Logo | Supplied externally from the prompt in 11.4 |
| Retention | Ready jobs are kept 7 days by default (jobs.retention = 168h), then deleted from Mountenant and the backend |
| Duplicate NZBs across users | Not shared: each user's job is imported separately into its own job directory; deduplication applies only within one user |
| Backend namespace | One category per instance, one directory per job (ADR 0002) |
| Range handling | Owned by Mountenant; only closed in-bounds ranges reach the backend (ADR 0003) |
| Backend deletion | Verified, never inferred from response codes (ADR 0004) |
| NZB intake | Standard DOCTYPE accepted; `.nzb.gz` decompressed with a bounded stream; digest over decompressed XML (ADR 0005) |
| Backend versions | Settled by the phase-2 case study (`docs/feasibility.md`): AltMount 0.3.2 (`14ad0f5`) and NzbDav 0.6.4, with the required settings in 10.5 |
| Failed job retention | Failed jobs expire after 24 h (jobs.failedRetention); every non-Deleted job has an expiry |
| Backend drift | Ready jobs re-verified hourly; missing content moves the job to Failed; an orphan sweep removes backend items without a job |
| Duplicate upload | Existing job returned unchanged with its original name; Failed jobs can be retried by re-uploading |
| Nested backend folders | Kept as-is in RelPath, never flattened; downloads use the base name |
| Login memory | At most 2 concurrent argon2id hashes; rate limit checked before hashing |
