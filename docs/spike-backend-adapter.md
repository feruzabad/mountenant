# Mountenant — Backend Adapter Spike

Oct 7, 2026 · implements specification revision 2 §10 and ADRs 0002–0004 and 0006, run against live AltMount 0.3.2 and NzbDav 0.6.4 test instances (see `docs/feasibility.md`)

## 1. Goal and result

**Goal.** Check that a single `sabdav` adapter with two profiles can run both backends through the whole job lifecycle, using the code Mountenant will actually ship instead of curl. Also check that the streaming proxy's own Range handling keeps AltMount from destroying files (feasibility §4.4).

**Result: it works on both backends.** The final live runs passed on both:

- Submit, Find, Status, ListFiles, ListNamespace, Delete and VerifyGone all behaved as the spec says.
- All 8 cases of the Range test table passed through the proxy. This includes the suffix range and the range past the end of the file, which each destroyed a file when sent to AltMount directly; afterwards the file was still listed and readable.
- 60/60 random reads and 16/16 full downloads were byte-identical to the reference ISO (SHA-256 `7a70c16e…`).
- Failing NZBs ended as `Failed` with the backend's message.
- Every job the test created was deleted and confirmed gone.

**Two findings changed the design:**

1. **AltMount sometimes ends a download early.** It sends `200` with the full `Content-Length`, then closes the connection part-way through. Plain curl hit this in 2 of 9 full downloads, and the proxy saw 3 cut-offs across 8 downloads. The proxy now reopens the backend from the current position and keeps going (ADR 0006). With that, all 8 downloads finished with correct bytes, and the client never saw the cut-offs.
2. **NzbDav's delete is idempotent when called the way the profile calls it.** With `del_completed_files=1`, deleting an unknown id returns `200`. Only the plain form returns 500. Feasibility §4.5 and spec §10.2 are corrected.

## 2. What was built

The code follows the spec's layout (§13.1) and is meant to be kept, not thrown away. It uses the standard library only.

| Path | Lines | Purpose |
| --- | --- | --- |
| `go.mod` | | Module `mountenant`, Go 1.27 |
| `internal/jobs/domain/backend.go` | 131 | `Backend` port, value types (`BackendRef`, `BackendStatus`, `BackendFile`, `ByteRange`, `NamespaceListing`), sentinel errors |
| `internal/jobs/adapters/sabdav/profile.go` | 86 | `AltMount` and `NzbDav` profiles: API path, content root, readiness mode, delete parameters, status map |
| `internal/jobs/adapters/sabdav/adapter.go` | 374 | Generic adapter with every port method; retries, URL building, path validation |
| `internal/jobs/adapters/sabdav/sab.go` | 229 | SABnzbd client: both error styles, multipart submit, queue and history reads, single-shot mutations |
| `internal/jobs/adapters/sabdav/dav.go` | 213 | WebDAV client: PROPFIND parsing (both href forms), DELETE, GET with response validation |
| `internal/delivery/adapters/streamproxy/byterange.go` | 90 | RFC 9110 Range/If-Range resolution against the catalogue (ADR 0003) |
| `internal/delivery/adapters/streamproxy/proxy.go` | ~190 | Streaming proxy: its own validators and headers, errors before the first byte, transparent upstream resume (ADR 0006) |
| unit tests | ~700 | Per-profile fake backends, range table, proxy behaviour, no network |
| `internal/jobs/adapters/sabdav/live_test.go` | ~410 | Live contract test (build tag `live`) |

Roughly 1 300 lines of code and 1 100 lines of tests.

### 2.1 The port as implemented

```go
type Backend interface {
    Ping(ctx) error                                              // /readyz
    Submit(ctx, nzb []byte, jobID string) (BackendRef, error)    // not idempotent
    Find(ctx, jobID string) (BackendRef, bool, error)            // before every re-submit
    Status(ctx, BackendRef) (BackendStatus, error)
    ListFiles(ctx, BackendRef) ([]BackendFile, error)
    Open(ctx, BackendRef, relPath string, *ByteRange) (io.ReadCloser, error) // nil or a closed in-file range
    Delete(ctx, BackendRef) error                                // outcome decided by VerifyGone
    VerifyGone(ctx, BackendRef) (bool, error)
    ListNamespace(ctx) (NamespaceListing, error)                 // orphan sweep, includes the backend clock
}
```

This is spec §4.5 plus `Ping`, which `/readyz` needs and the spec doesn't list yet (§4).

Design points, each traceable to the study:

- **Errors** are typed sentinels (`ErrBackendUnavailable`, `ErrBackendRejected`, `ErrContentMissing`, `ErrBadResponse`). Only `ErrBackendUnavailable` is retried: 3 attempts, exponential backoff with jitter. SAB responses are checked on their JSON `status` field, because AltMount reports errors with HTTP 200. HTTP 5xx and transport errors count as unavailable.
- **Mutations are never retried automatically**: addfile, queue delete and history delete. A blind retry could duplicate a job or hide an error.
- **`Status`** reads the history, then the queue, then the history again if the job wasn't found. This way a job that moves from the queue into the history between the two calls isn't reported missing. When AltMount has duplicate history rows, the terminal one wins.
- **`ListFiles`** keeps only the decoded URL path of each `href`, so it handles NzbDav's absolute URLs pointing at its internal host. It treats an entry as a directory based on `resourcetype`, and rejects any path outside the job directory or containing `..`.
- **`Open`** sends only `bytes=a-b` and checks the response: status, exact `Content-Range` and `Content-Length`. It never follows redirects (AltMount answers path tricks with 307).
- **`Delete`** removes the job from the queue only if it is actually queued. It then deletes the history row, adding the profile's parameters, and sends a WebDAV `DELETE` on the job directory. All errors are collected; `VerifyGone` decides the outcome.
- **The proxy** derives `ETag` from job ID, path and size. `Last-Modified` is `ReadyAt`. `Content-Type` comes from the file extension, and `Content-Disposition` is built with `mime.FormatMediaType`, which RFC 2231-encodes non-ASCII names. It copies with a fixed 128 KiB buffer. It never sends a backend status code or header to the client.

## 3. Test results

### 3.1 Unit tests (`go test -race ./...`, no network)

All 13 top-level tests pass, 35 cases including subtests.

| Test | What it checks |
| --- | --- |
| `TestResolveRange` (18 cases) | Closed, open-ended, past-EOF clamping, suffix (also larger than the file and zero), unsatisfiable, multi-range → whole file, unknown unit and bad syntax ignored, `If-Range` with strong, weak and stale ETags and with dates. The result never leaves the file. |
| `TestServeForwardsOnlyClosedRanges` | The backend receives only `nil` or closed in-file ranges, for suffix, past-EOF, open-ended and multi-range client requests |
| `TestServeUnsatisfiableAndHeadDoNotCallBackend` | 416 with `bytes */size` and HEAD are answered without calling the backend |
| `TestServeHeaders` | Content type, RFC 6266 disposition, validators, `Cache-Control: private, no-store` |
| `TestServeErrorsBeforeFirstByte` | `ErrContentMissing` → 410 plus the content-missing hook; other errors → 502 |
| `TestServeResumesTruncatedUpstream` | A backend that ends early, cleanly or with an error, is reopened at exactly the next byte; the body stays byte-identical |
| `TestServeAbortsMidStream` | Once resumes run out, the client connection is reset, so a short download can't look complete |
| `TestLifecycle` (×2 profiles) | Fakes that reproduce each product's quirks (error style, href form, delete semantics) go through the full lifecycle. The fakes reject any non-closed range. |
| `TestVerifyGoneSeesLeftoverContent` | A missing history row with a remaining directory counts as not gone; `Delete` then falls back to WebDAV `DELETE` |
| `TestErrorStyles` (×2) | A wrong API key gives `ErrBackendRejected` on both, despite AltMount's HTTP 200 |
| `TestOpenRetriesTransient500` | Two 500s are retried and succeed; a lasting 500 gives `ErrBackendUnavailable` |
| `TestOpenRejectsUnsafeInput` | `..`, absolute, empty and double-slash paths and negative ranges are rejected before any request |
| `TestParseResponseHrefForms` | Path-only and absolute hrefs, percent-decoding |

### 3.2 Live runs

Each run used one Debian job per backend, plus the two failing NZBs (`missing.nzb` and CentOS). Logs were kept outside the repository and are not committed.

| Run | Scope | Outcome |
| --- | --- | --- |
| 1 | both backends, 1 full download each | NzbDav passed. AltMount failed on the full download: the backend body ended after 8.4 MB. The proxy reset the connection correctly, as designed then, but the download failed. This led to finding 1. |
| 2 | both backends, 4 full downloads each, with ADR 0006 resume | **Passed**: 8/8 downloads byte-perfect, no cut-offs this time |
| 3 | AltMount only, 8 full downloads | **Passed**: 3 cut-offs at 102.7 MB, 15.5 MB and 91.6 MB, all resumed transparently; 8/8 SHA-256 correct |

Lifecycle, from run 2:

| Step | AltMount | NzbDav |
| --- | --- | --- |
| `Ping` | ok (`mode=version` with key + PROPFIND) | ok (`mode=queue` with key + PROPFIND) |
| `Find` unknown / after submit | not found / found, same nzo_id | not found / found, same nzo_id |
| nzo_id | = jobId | random GUID |
| Status sequence (Debian) | Queued 0 s → Importing 6 s → Completed 9 s | Importing 0 s → Completed 3 s |
| `ListFiles` | `debian-10.0.0-arm64-netinst.iso`, 262 144 000 B | same |
| `ListNamespace` | 1 dir, 1 history row | same |
| Backend clock vs. real UTC | +1 h 50 min | +1 h 50 min |
| Delete → `VerifyGone` | gone on 1st check; repeat delete no error | gone on 1st check; repeat delete no error |
| `missing.nzb` | Failed after 6 s: `fast-fail segment check failed: …` | Failed after 1 s: `Article with message-id … not found.` |
| CentOS (compressed RAR) | Failed after 16 s: `compressed files are not supported: … (uses rar2.9 compression)` | Failed after 7 s: `Only rar files with compression method m0 are supported.` |
| Adapter retries (whole run) | 0 | 0 |

Range table through the proxy. Identical on both backends in every run:

| Client request | Proxy → client | Sent to backend | Bytes correct |
| --- | --- | --- | --- |
| `bytes=-1024` | 206 `262142976-262143999` | closed range | yes |
| `bytes=<size-10>-<size+100>` | 206 `262143990-262143999` | clamped closed range | yes |
| `bytes=<size-1000>-` | 206 | closed range | yes |
| `bytes=32768-33791` | 206 | same | yes |
| `bytes=<size>-` | 416 `bytes */262144000` | nothing | n/a |
| `If-Range` with matching ETag | 206 | closed range | yes |
| `If-Range` with stale ETag | 200 whole file | no range | yes (length) |
| `bytes=0-9,20-29` | 200 whole file | no range | yes (length) |
| after the table | file still listed and readable on AltMount | | yes |

Throughput and latency through the proxy:

| | AltMount | NzbDav |
| --- | --- | --- |
| 20 random 64 KiB / 1 MiB ranges, 4 parallel | 20/20 correct, mean 0.49 s, max 0.75 s | 20/20 correct, mean 1.48 s, max 2.60 s |
| Full download, 262 MB (runs 2+3, 12 / 4 runs) | 4.5–7.9 MB/s | 4.3–7.2 MB/s |
| Client abort after 1 MiB | handler returned immediately, upstream cancelled | same |

The feasibility study measured about 7.6 MB/s for a direct WebDAV download. The proxy's best runs (7.8 / 7.2 MB/s) are within a few percent of that. The spread between runs follows backend and provider speed (the slowest AltMount runs were the ones that resumed), not proxy overhead. The spec's "< 5 % overhead" target needs a dedicated A/B benchmark, which this spike didn't do.

## 4. Findings

1. **AltMount ends full downloads early, intermittently (new).** The backend sends `200` with the full `Content-Length` and closes the connection part-way, with nothing in AltMount's info-level log. Plain curl, with no Mountenant involved, reproduced it in 2 of 9 full downloads (curl exit 18, "partial file"). An aborted download beforehand made no difference. The proxy saw it in 1 of 4 downloads in run 1 and 3 times in 8 downloads in run 3. AltMount's provider error counter rose from 21 to 167 over the session, so the likely cause is article fetch errors that AltMount turns into end-of-file instead of retrying. Unlike the Range bug, this doesn't mark the file corrupted; the next request works. **Mitigation (ADR 0006):** the proxy reopens the backend at `[sent, end]` up to `MaxResumes` (3) times per response and only resets the client connection if that fails too. The live evidence is 3/3 resumes successful and byte-perfect. Upstream: not investigated or reported yet (§6).
2. **Range normalisation protects AltMount, as designed.** The suffix and past-EOF requests that permanently hid a file when sent directly were served correctly through the proxy, and the file stayed. No health record was created in any run.
3. **NzbDav delete is idempotent with `del_completed_files=1`.** An unknown or already-deleted id returns `200 {"status":true}` with that parameter, and `500` only without it, which is how the feasibility study called it. The design (verify, don't trust the response) is unchanged; the docs now say the profile's call is idempotent.
4. **No transient 5xx this time.** The adapter retried 0 times across all three runs. The early NzbDav 500s from the feasibility study didn't come back. The retry path is covered by unit tests only.
5. **The backend clock is about 1 h 50 min ahead of UTC on both instances.** Same as in the study. `ListNamespace` returns the backend clock from the `Date` header so the orphan sweep can compare like with like.
6. **Imports are fast and failures are quick.** Debian took 3–9 s, compressed CentOS failed in 7–16 s, and missing articles failed in 1–6 s. This supports the spec's 30-minute `importTimeout` default as a generous upper bound.
7. **The port needs `Ping`.** `/readyz` needs a backend check that validates both the API key and the WebDAV credentials. Spec §4.5 should list it.

## 5. How to run

```sh
# unit tests
go test -race ./...

# live contract test (a backend is skipped if its URL is unset)
export MT_ALTMOUNT_API_URL=http://<altmount-host>:8080 MT_ALTMOUNT_API_KEY=… MT_ALTMOUNT_DAV_USER=usenet MT_ALTMOUNT_DAV_PASS=…
export MT_NZBDAV_API_URL=http://<nzbdav-host>:3000   MT_NZBDAV_API_KEY=…   MT_NZBDAV_DAV_USER=admin  MT_NZBDAV_DAV_PASS=…
export MT_LIVE_NZB=/path/to/Debian.10.Netinstall.iso.nzb
export MT_LIVE_NZB_FAIL="/path/to/missing.nzb:/path/to/CentOS-7-x86_64-DVD-1810.nzb"   # optional, ':'-separated
export MT_LIVE_REF=/path/to/debian-10.0.0-arm64-netinst.iso                                  # optional byte reference
export MT_LIVE_SHA256=7a70c16ebb50e173f27a5cb4ddbb971e8848a4fbccb2187b827931f24f31ac4c
export MT_LIVE_FULL=4                                                               # full downloads per backend
go test -tags live -v -count=1 -timeout 40m ./internal/jobs/adapters/sabdav -run TestLive
```

The live test cleans up after itself: every job it creates is deleted and confirmed gone, and both instances were left with empty history and an empty namespace. It needs the backend settings in spec §10.5. Go 1.27 is required; any standard installation works.

## 6. Next steps

1. **Look at the AltMount cut-offs upstream**, the same way as the Range bug: search the issue tracker, check whether `main` behaves differently, and report it if it's new. Include the curl reproduction, which needs no Mountenant code.
2. **Wire the adapter into the Jobs application layer**: the reconciler (UC-14), deletion with `VerifyGone` retries (UC-13), the orphan sweep (UC-16), and the submit retry that calls `Find`.
3. **Add a controlled overhead benchmark**: direct WebDAV and proxy in alternating runs, same file, to check the < 5 % target in spec §12.1.
4. **Expose the counters** `Adapter.Retries()` and `Proxy.Resumes()` as Prometheus metrics (spec §12.3), so cut-off rates show up in production.
5. **Run the live test nightly** against the pinned containers (spec §13.4).
