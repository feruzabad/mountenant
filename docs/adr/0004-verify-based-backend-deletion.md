# 4. Verify-based backend deletion

Date: 2026-10-07

## Status

Accepted

## Context

The phase-1 specification defines `Delete` as a SAB history delete (and/or
WebDAV `DELETE`, per Profile), idempotent, with 404 treated as success (§10.2).
The feasibility study found that neither backend removes content with the
standard call, and that their response codes cannot be trusted:

| | AltMount 0.3.2 | NzbDav 0.6.4 |
| --- | --- | --- |
| `mode=history&name=delete&value=<nzo>` | Removes the history row only; `del_files` ignored, content stays on WebDAV. Always `status:true`, also for unknown ids | Removes the history row; `del_files` ignored. Non-standard `del_completed_files=1` removes the content. Unknown or already deleted id → HTTP 500 |
| WebDAV `DELETE` on the Job directory | 204, repeat → 204 | 403 while `webdav.enforce-readonly=true`; otherwise 200, repeat → 404 |

AltMount reports success for every SAB error with HTTP 200. NzbDav's 500 is
returned both for an unknown id and for real failures (database lock, disk
error), and the two cannot be told apart from the response. AltMount also kept
reporting `Completed` in history for a Job whose file was gone from WebDAV.

Rejected alternative: decide success from response codes (treat 2xx and 404 or
500 as "gone"). That either deletes nothing on AltMount while reporting success,
or hides real NzbDav failures and leaves content on the shared provider account.

## Decision

1. **Profile-specific delete calls.**
   - AltMount: `mode=history&name=delete&value=<nzo>`, then WebDAV `DELETE` on
     `/webdav/complete/<cat>/<jobId>/`. Both are idempotent.
   - NzbDav: `mode=history&name=delete&value=<nzo>&del_completed_files=1`.
     In-progress items use `mode=queue&name=delete`.
2. **Success is decided by verification, never by a response code.** The
   response only selects the next step. A delete is complete when both hold:
   - SAB `mode=queue&nzo_ids=<nzo>` and `mode=history&nzo_ids=<nzo>` both
     return no slot (covers Jobs deleted while still queued; verified on
     NzbDav: a deleted GUID gives 200 with empty `slots`, not 500), **and**
   - `PROPFIND Depth: 0` on the Job directory returns 404.
   If the history row is gone but the directory remains, the adapter sends a
   WebDAV `DELETE` (on NzbDav this needs `webdav.enforce-readonly=false`) and
   verifies again.
3. **Retries by the reconciler.** Until verification succeeds the Job stays
   Deleted with backend cleanup pending; the reconciler retries with backoff and
   alerts after N consecutive failures. The Deleted row is kept for audit either
   way.
4. **Orphan sweep as safety net** (UC-16). It lists the category directory
   (`PROPFIND Depth: 1`) and SAB history by category, and deletes entries whose
   name is not a non-Deleted Job ID (including `<jobId> (n)` duplicates). Three
   rules make it race-safe:
   - **Persist before submit**: UC-10 commits the Job row (Queued) before
     calling `Submit`, so every backend item named `<jobId>` has a row when the
     backend creates it.
   - **Read DB IDs after listing**: the sweep lists the backend first and loads
     Job IDs afterwards; any Queued, Importing, Ready or Failed Job protects its
     directory.
   - **Minimum age**: entries younger than a grace period (e.g.
     `importTimeout`) are skipped, measured against the backend's own `Date`
     header because backend clocks are skewed. Names that are not Mountenant
     Job IDs are left alone (operator items).
   The sweep runs every `orphanSweepInterval` and 10 minutes after startup,
   never concurrently with itself, and starts in `orphanSweepDryRun`.

## Consequences

- Deletion is correct on both backends despite different, non-standard
  mechanisms and unreliable status codes; `PROPFIND` 404 is the single
  authoritative "gone" signal.
- Each delete costs two extra read requests; this is negligible.
- On NzbDav the operator must set `webdav.enforce-readonly=false` for the
  WebDAV fallback and the sweep; this joins the documented prerequisites.
- A Deleted Job can have content on the backend for a while; metrics and logs
  must make pending cleanups visible.
- Specification §10.2 `Delete` ("Idempotent; 404 treated as success"), UC-13,
  UC-15 and UC-16 must be updated.

References: `docs/feasibility.md` §1 (#3), §3.2, §4.2, §4.5, §4.6, §4.8, §6, §7;
`docs/specification.md` UC-10, UC-13, UC-15, UC-16, §10.2, §10.4.
