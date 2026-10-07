# 3. Mountenant owns HTTP range semantics

Date: 2026-10-07

## Status

Accepted

Decision 5, errors after the first byte: superseded by ADR-0006 (transparent
upstream resume).

## Context

UC-21 promises `Range` / `If-Range` support, and the phase-1 specification has
`Open` forward these headers to the backend's WebDAV `GET` (§10.2). The
feasibility study ran an RFC 9110 §14 conformance matrix against both backends.
Closed in-bounds ranges (`bytes=a-b`) and open-ended ranges (`bytes=a-`) were
correct on both, and 60 random ranges matched the full file byte for byte.
Everything else was not:

| Request | AltMount 0.3.2 | NzbDav 0.6.4 |
| --- | --- | --- |
| `bytes=-N` (suffix) | 206 headers, empty body, then the file is marked corrupted and returns 404 permanently | 206 with the wrong range `bytes 0-N` |
| `bytes=a-b`, b ≥ size | 206 headers, empty body, file marked corrupted (404) | clamped correctly |
| `bytes=0-9,20-29` | 206 multipart | first range only |
| `If-Range: <etag>` | 200 with full `Content-Length` but only the range bytes | no ETag support |
| `If-Range: <date>`, not matching | 200 with full `Content-Length`, truncated body | 206, validator ignored |

On AltMount the corruption mark is sticky while its health system is disabled
(the default): the file disappears from WebDAV while SAB history still says
`Completed`, and the Job must be re-imported. The study traced it to
`ParseRangeHeader` returning `Start=-1` for suffix ranges and reproduced both
cases deterministically on separate files.

Upstream history (checked 2026-10-07) shows an accidental regression, not an
accepted behaviour. `FixRangeHeader` normalised suffix and past-EOF ranges from
2025-08-10; refactor commit `5450215` (2025-08-19) dropped its call, and PR #659
(2026-06) deleted it as dead code. v0.3.2 (2026-07-18, still the latest release)
ships both bugs. PR #937 (merged on `main` 2026-09-08, unreleased) fixes the
past-EOF case; PR #976 (opened 2026-10-06, open and unreviewed) fixes suffix
ranges, so suffix ranges are still broken even on `main`.

Neither backend sends a usable `ETag` on `GET` (NzbDav has none at all), NzbDav
reports every file as `application/octet-stream`, backend clocks were about two
hours off, and NzbDav sends `Set-Cookie`, `Server: Kestrel` and
`X-Powered-By: Express`. NzbDav also returned intermittent 500s before the first
byte early in the study.

Rejected alternative: forward `Range` / `If-Range` verbatim. A single browser
request with a suffix range (common in media players) would destroy a User's
file on AltMount, and NzbDav would serve wrong bytes as a successful 206.

## Decision

1. Mountenant evaluates `Range` and `If-Range` itself against the catalogued
   `JobFile.Size` and its own validators, with Go's `http.ServeContent` logic as
   the reference. Mountenant owns `ETag` (derived from `jobId`, `RelPath` and
   size) and `Content-Type` (derived from the extension); backend values are
   ignored.
2. The adapter forwards **only** `Range: bytes=a-b` with `0 ≤ a ≤ b ≤ size-1`,
   or no `Range` at all. Suffix and open-ended ranges are rewritten to that
   form. `If-Range`, `If-None-Match` and `If-Modified-Since` are never
   forwarded.
3. Unsatisfiable ranges get 416 with `Content-Range: bytes */size` from
   Mountenant without a backend call. Multi-range requests are answered without
   forwarding: Mountenant ignores the `Range` header and serves the full file
   with 200, which RFC 9110 §14.2 permits. Download managers and media players
   send single ranges, so this costs nothing in practice and avoids
   `multipart/byteranges`.
4. The upstream response is validated: 206 with the expected `Content-Range`
   (or 200 when no range was sent) and the expected `Content-Length`.
5. Errors **before the first byte** (5xx, 404, failed validation): retry 5xx per
   §10.4, then 502 `backend_unavailable`; a 404 answers 410 `content_missing`
   and moves the Job to Failed `backend_content_missing`. The upstream status is
   never passed through. Errors **after the first byte** (drop, short body, read
   timeout): abort the client connection (`panic(http.ErrAbortHandler)`) so the
   client sees a short body and resumes with a new, normalised range request.
6. Only an allowlist of upstream headers reaches the client.

## Consequences

- The decision stands regardless of upstream fixes: the pinned AltMount release
  has both bugs, the suffix fix (#976) is not merged, and NzbDav has its own
  suffix-range bug. Even with fixed backends it remains defence in depth for a
  destructive failure mode and gives consistent validators.
- When an AltMount release contains both #937 and #976, re-run the range
  conformance matrix of `docs/feasibility.md` §4.4 against it before re-pinning.
- Mountenant becomes the single place where range semantics are tested; the
  per-profile contract tests assert that only closed in-bounds ranges reach the
  fake backend.
- A backend error can never become a "successful" truncated download.
- A file can still be marked corrupted on AltMount for other reasons (e.g.
  articles removed from the provider); Ready re-verification (UC-14) and the
  Ready → Failed transition remain necessary.
- Specification §10.2 `Open`, UC-21 and §15.1 need updating; AltMount stays
  pinned at 0.3.2 (`14ad0f5`) until a release with #937 and #976 passes the matrix.

References: `docs/feasibility.md` §1 (#2), §4.3, §4.4 (incl. "Upstream history"), §5 (items 3, 4), §6, §7;
`docs/specification.md` UC-21, §10.2, §10.3, §10.4, §15.1.
