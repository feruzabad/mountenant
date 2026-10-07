# Mountenant — Backend Feasibility Report

Oct 7, 2026 · phase-2 case study for `docs/specification.md` (§10, §15.2 "Backend versions")

## 1. Verdict

**Feasible, with changes to the specification.** Both AltMount and NzbDav can drive every `Backend` port operation (§4.5) through the SABnzbd API plus WebDAV, and both produced byte-identical output for the same NZB. The "one generic `sabdav` adapter + thin per-product `Profile`" design holds, but the profiles carry more behaviour than §10.1 assumes, and four spec assumptions are wrong as written:

| # | Spec assumption | Finding | Severity |
| --- | --- | --- | --- |
| 1 | Per-user backend category `mt-<userId>` (§7.5, §10.2, UC-16) | AltMount rejects categories that are not pre-configured; creating them needs AltMount's private config API | **Blocking → change design** (single category, one directory per job) |
| 2 | `Range` / `If-Range` can be forwarded to WebDAV (§10.2 `Open`) | AltMount **permanently hides a file** after one suffix range (`bytes=-N`) or one range ending past EOF; NzbDav serves wrong bytes for suffix ranges; neither handles `If-Range` correctly | **Critical → Mountenant must normalise ranges** |
| 3 | `Delete` = SAB history delete (§10.2) | Neither backend removes content with the standard call; each needs a different mechanism | High → profile-specific delete |
| 4 | DOCTYPE declarations are rejected (§7.6) | Real-world NZBs (both test files) carry the standard newzbin `<!DOCTYPE>` | High → accept DOCTYPE, reject internal subsets/entities |

Hard product limits Mountenant cannot work around (they are inherent to "streaming without download"):

- **Compressed RAR archives are unsupported by both backends.** Only store-mode (`-m0`) RARs, plain files, and similar are mountable. The CentOS NZB failed on both (§5.3). This affects a meaningful share of non-video Usenet content and must be communicated to users.
- **`.nzb.gz` is not accepted by either backend.** Mountenant must decompress it before submitting (it already parses the XML, so this costs nothing extra).

Everything else (intake, status, listing, streaming, resume, cancellation, health) works as the spec expects. Details and exact procedures follow.

> **Follow-up:** the backend adapter spike (`docs/spike-backend-adapter.md`) implemented this design in Go and ran it against both instances. It confirms the findings, corrects NzbDav's delete idempotency (§4.5), and found that AltMount intermittently ends full downloads early. That is handled by transparent upstream resume (ADR 0006).

## 2. Test environment

| | AltMount | NzbDav |
| --- | --- | --- |
| URL | `http://<altmount-host>:8080` (private test network) | `http://<nzbdav-host>:3000` (private test network) |
| Version | **0.3.2** (`kipsilabs/altmount`, commit `14ad0f5`) | **0.6.4** (`nzbdav-dev/nzbdav`, tag `v0.6.4`) |
| SABnzbd API reports | `4.5.0` | `4.5.1` |
| SAB API path | `/sabnzbd/api` (also `/sabnzbd`) | `/api` |
| WebDAV root | `/webdav/` | `/` (same port, served by the backend behind the Express UI) |
| Provider | one commercial Usenet provider, 50 connections (same account on both) | same |
| UI login | local admin account | local admin account |

Test data:

| NZB | Content | Result |
| --- | --- | --- |
| `Debian.10.Netinstall.iso.nzb` (73 KB, 14 files, 828 segments) | 6-part store-mode RAR + PAR2, containing `debian-10.0.0-arm64-netinst.iso` (262 144 000 B) | Ready on both after configuration; SHA-256 `7a70c16e…f31ac4c` identical from both backends |
| `CentOS-7-x86 64-DVD-1810.nzb` (1.5 MB, 96 files, 13 253 segments, ~5 GB) | Compressed RAR (rar2.9) | **Failed on both** (unsupported compression) |
| `missing.nzb` (crafted, 3 bogus message-ids) | n/a | Failed on both with a clear message |
| `debian.nzb.gz` (gzip of the Debian NZB) | n/a | Not accepted by either |

Raw evidence (scripts, request/response captures, downloaded files) was kept outside the repository and is not committed.

## 3. Backend setup changes (operator checklist)

These are the settings a Mountenant deployment needs on each backend. Each was applied to the test instances, with the before → after values listed.

### 3.1 AltMount 0.3.2

| Setting | Before | After | Why |
| --- | --- | --- | --- |
| `sabnzbd.enabled` | `false` | `true` | SAB API returns 404 otherwise |
| `sabnzbd.categories` | Movies, TV, Music, Books, Adult | + `mountenant` | Unknown categories are rejected (§4.1) |
| `webdav.password` (user `usenet`) | unknown | set by the operator | WebDAV credentials for Mountenant |
| `import.allowed_file_extensions` | 23 media extensions | `[]` (= allow all) | Otherwise `.iso`, `.zip`, `.exe` etc. fail: `archive contains no files with allowed extensions (found: [.iso] …)` |
| `import.expand_bluray_iso` | unset (= `true`) | `false` | Otherwise **every `.iso` is replaced by the largest file inside it**, renamed after the release (Debian ISO became a 40 MB `Debian.10.Netinstall.iso.DEB`) |
| `import.rename_to_nzb_name` | unset (= `true`) | `false` | Otherwise single files are renamed to the NZB name (`<jobId>.iso` instead of `debian-10.0.0-arm64-netinst.iso`) |

`expand_bluray_iso` and `rename_to_nzb_name` are **not exposed in the AltMount UI**. Set them in `config.yaml` or with the config API:

```sh
# login (JWT cookie, rate limited 10/min)
curl -c cj -X POST $AM/api/auth/login -H 'Content-Type: application/json' \
     -d '{"username":"<user>","password":"<password>"}'          # response also contains the API key
# PATCH merges the body into the full config; wrap fields in their section name
curl -b cj -X PATCH $AM/api/config/import -H 'Content-Type: application/json' \
     -d '{"import":{"allowed_file_extensions":[],"expand_bluray_iso":false,"rename_to_nzb_name":false}}'
curl -b cj -X PATCH $AM/api/config/sabnzbd -H 'Content-Type: application/json' \
     -d '{"sabnzbd":{ …full section with enabled:true and categories incl. "mountenant"… }}'
```

Other notes:

- The SAB API key is the admin user's API key (`GET /api/user` → `api_key`). Regenerating it on the user page changes it.
- AltMount's health system is disabled by default. Files marked corrupted then stay hidden; there is no automatic recovery (§5.4).

### 3.2 NzbDav 0.6.4

| Setting | Before | After | Why |
| --- | --- | --- | --- |
| `api.key` | pre-generated | unchanged | SAB API key |
| `webdav.user` / `webdav.pass` | `admin` / empty | `admin` / set by the operator | WebDAV credentials (stored hashed) |
| `api.ensure-importable-video` | `true` | `false` | "Fail downloads for nzbs without video content". Otherwise every non-video NZB fails |
| `webdav.enforce-readonly` | `true` | `false` | **Required.** Mountenant deletes via WebDAV in the orphan sweep (§4.6) and in the delete fallback when a history row is gone but the directory remains (§4.5); with `true`, `DELETE` returns 403 |
| `api.categories` | empty | unchanged | Empty list = any category is accepted |
| `api.duplicate-nzb-behavior` | `increment` | unchanged | See §4.1 duplicates |
| `api.import-strategy` | `symlinks` | unchanged | Irrelevant for Mountenant (it reads `/content`) |

Settings are changed via the UI (Settings → SABnzbd / WebDAV) or the UI's own endpoint:

```sh
curl -c cj -X POST $ND/login --data 'username=<user>&password=<password>'          # session cookie
curl -b cj $ND/settings.data                                              # current config (turbo-stream JSON)
curl -b cj -X POST $ND/settings/update -F 'config={"api.ensure-importable-video":"false","webdav.pass":"…","webdav.enforce-readonly":"false"}'
```

## 4. Port operations — how to perform them

`$AM_API=http://host:8080/sabnzbd/api`, `$AM_DAV=http://host:8080/webdav`, `$ND_API=http://host:3000/api`, `$ND_DAV=http://host:3000`. All SAB calls add `output=json&apikey=$KEY`.

### 4.1 `Submit(nzb, namespace)`

```sh
# AltMount
curl -X POST "$AM_API?mode=addfile&output=json&apikey=$K&cat=mountenant&nzbname=$JOB_ID" \
     -F "name=@job.nzb;filename=$JOB_ID.nzb;type=application/x-nzb"
# → {"status":true,"nzo_ids":["<JOB_ID>"]}

# NzbDav
curl -X POST "$ND_API?mode=addfile&output=json&apikey=$K&cat=mountenant" \
     -F "name=@job.nzb;filename=$JOB_ID.nzb;type=application/x-nzb"
# → {"nzo_ids":["<random GUID>"],"status":true,"error":null}
```

| Aspect | AltMount | NzbDav |
| --- | --- | --- |
| Multipart field | `nzbfile` or `name` | `nzbFile` or `name` |
| `cat` must exist | **yes**: `invalid category '…' - not found in configuration` | no, any string is accepted and creates the directory |
| `nzbname` | Used as the **nzo_id** (if absent, a random UUID) | **Ignored**; nzo_id is a random GUID |
| Job directory name | Multipart **filename** without `.nzb` | Multipart **filename** without `.nzb` |
| Display `name` in history | filename stem | filename stem |
| Without our filename | NZB `<meta name>` / original filename (`Debian.10.Netinstall`) | original filename (`Debian.10.Netinstall.iso`) |
| `.nzb.gz` | Accepted, then fails: `XML syntax error … U+001F` | Rejected: `'\u001F' … invalid character` |
| Duplicate (same filename, same category) | Accepted; merged into the **same directory**, two history rows with the same nzo_id | New job with directory `<name> (2)` (`increment` setting) |
| Time to Completed (Debian, 260 MB) | ~6 s (metadata import only, no download) | ~1–2 s |
| Import concurrency | `import.max_processor_workers` = 2 | serial (one in-progress queue item) |

**Queue capacity.** An import only reads NZB metadata and samples segments; no content is downloaded. So the per-job cost is seconds, not minutes: Debian 1–6 s, failing CentOS ~5 s. Even with serial imports on NzbDav, 30 simultaneous submissions clear in roughly one to three minutes. Mountenant needs no extra state for this: a waiting job shows the backend status `Queued` and maps to Mountenant `Queued`, and only the active item maps to `Importing`. The UI should show `Queued` as "waiting for backend". `importTimeout` has to cover queue wait plus import, so measure on the target hardware and keep the default at least 30 min. Large multi-volume releases take longer to import (more segments to sample); this was not measured here.

**Recommended Mountenant behaviour.** Always send the multipart filename `<jobId>.nzb` and `nzbname=<jobId>`. Then on both backends the job directory is `<category>/<jobId>/`, which is fully traceable. Store the returned `nzo_ids[0]` in `BackendRef`. Always gunzip before submitting. Do it as a bounded stream: `io.LimitReader(gzip.NewReader(body), maxNzbBytes+1)`, and reject the upload with 413 `nzb_too_large` if the limit is hit. Memory is then capped at the per-user NZB quota no matter the compression ratio, and the decompressed NZB fits in the `nzb_blobs` row the spec already keeps until the backend accepts it. "No local storage" (§1.1 G3) covers content bytes, not the NZB. Before a retry of a submission (§10.4), look the job up by name (`mode=queue` + `mode=history`, match `name == jobId`) to avoid `(2)` duplicates on NzbDav and merged directories on AltMount.

### 4.2 `Status(ref)`

```sh
curl "$AM_API?mode=queue&output=json&apikey=$K"
curl "$AM_API?mode=history&output=json&apikey=$K&nzo_ids=$NZO"   # by-id lookup ignores history retention window
curl "$ND_API?mode=queue&output=json&apikey=$K&category=mountenant"
curl "$ND_API?mode=history&output=json&apikey=$K&nzo_ids=$NZO"   # NzbDav: nzo_ids must be GUIDs
```

Check the queue first, then the history. Observed and source-confirmed status strings:

| Backend value | Where | AltMount meaning | NzbDav meaning | Mountenant status |
| --- | --- | --- | --- | --- |
| `Queued` | queue | pending | waiting | Queued |
| `Paused` | queue | paused | n/a | Queued (log) |
| `Downloading` | queue | processing (metadata import) | in progress | Importing |
| `Unknown` | **history** | item still pending/processing (seen while queued) | n/a | ignore; use queue |
| `Completed` | history | done; `storage` = `complete/<cat>/<jobId>` | done; `storage` = `/mnt/nzbdav/completed-symlinks/<cat>/<jobId>` | Importing → Ready after `ListFiles` returns ≥ 1 file |
| `Failed` | history | `fail_message` set; also synthesised when the path is missing | `fail_message` set | Failed `backend_failed` (message = `fail_message`) |

Observed `fail_message` values (good enough to show to users):

| Case | AltMount | NzbDav |
| --- | --- | --- |
| Compressed RAR | `compressed files are not supported: CentOS-7-x86_64-DVD-1810.iso (uses rar2.9 compression)` | `Only rar files with compression method m0 are supported.` |
| Missing articles | `fast-fail segment check failed: no regular files were successfully processed (all files failed validation)` | `Article with message-id …@… not found.` |
| Extension filter (misconfig) | `archive contains no files with allowed extensions (found: [.iso], …)` | (`ensure-importable-video`) |
| Bad NZB | `failed to parse NZB file: …` | rejected at submit (`status:false`) |

API error conventions differ. AltMount returns **HTTP 200** with `{"status":false,"error":…}` for every SAB error, including a wrong API key. NzbDav returns 401 for a wrong key, and **500** with an ASP.NET message for an unknown id or a missing key. The adapter must check the `status` field and must not rely on the HTTP code alone.

AltMount history retention: `sabnzbd.history_retention_minutes` = 10080 (7 days) for the list view. Lookups by `nzo_ids` bypass the window (source comment, issue #543). NzbDav keeps all history.

### 4.3 `ListFiles(ref)`

```sh
curl -u $USER:$PASS -X PROPFIND -H 'Depth: infinity' "$AM_DAV/complete/mountenant/$JOB_ID/"
curl -u $USER:$PASS -X PROPFIND -H 'Depth: infinity' "$ND_DAV/content/mountenant/$JOB_ID/"
```

| Aspect | AltMount | NzbDav |
| --- | --- | --- |
| Job directory | `/webdav/complete/<cat>/<jobId>/` (`complete` = `sabnzbd.complete_dir`) | `/content/<cat>/<jobId>/` (`/completed-symlinks/…` holds only `.rclonelink` stubs; ignore) |
| `Depth: infinity` | supported | supported |
| `href` form | path only, dirs end with `/` | **absolute URL with internal host** (`http://localhost:8080/content/…`), dirs **without** trailing `/`, spaces as `%20` |
| `getcontentlength` | yes | yes (dirs report `0`) |
| `getcontenttype` | by extension (`application/x-iso9660-image`) | always `application/octet-stream` |
| `getetag` | yes, in PROPFIND only (not on GET/HEAD) | **none** |
| `resourcetype` | `<D:collection/>` for dirs | `<D:collection />` for dirs |
| Other roots | `/webdav/complete/` only | `/.ids`, `/nzbs`, `/completed-symlinks`, `/content` |

Adapter rules: take only the URL path of each `href`, percent-decode it, strip the job-directory prefix to get `RelPath`, and detect directories via `resourcetype`, not via a trailing slash. Mountenant derives `ContentType` from the extension itself (NzbDav gives no useful type) and computes its own `ETag` (e.g. hash of `jobId + relPath + size`), because NzbDav has none and AltMount does not send its ETag on GET.

### 4.4 `Open(ref, relPath, byteRange)`

Both serve `GET` with `Accept-Ranges: bytes`, 206 + `Content-Range` for simple ranges, and correct bytes. Verification: full downloads hash-identical; 60 random 64 KiB / 1 MiB ranges compared against the full file (30 per backend, 4 parallel streams per backend, both backends at once): 60/60 correct. Single-stream throughput was ~7.6 MB/s on both (262 MB in ~35 s). Random-seek latency: AltMount p50 0.9 s / p95 6.5 s, NzbDav p50 1.6 s / p95 2.7 s.

Range conformance matrix (RFC 9110 §14):

| Request | AltMount 0.3.2 | NzbDav 0.6.4 |
| --- | --- | --- |
| `bytes=a-b` within file | 206 correct | 206 correct |
| `bytes=a-` (open-ended) | 206 correct | 206 correct |
| `bytes=-N` (suffix) | 206 headers, **empty body, then file marked corrupted and removed from WebDAV (404) permanently** | 206 with **wrong range** `bytes 0-N` (N+1 bytes from the start) |
| `bytes=a-b`, b ≥ size (must clamp) | 206 headers, **empty body, file marked corrupted (404)** | 206, clamped correctly |
| `bytes=a-b`, a ≥ size | 416 + `Content-Range: bytes */size`, file survives | 416 correct |
| `bytes=0-9,20-29` | 206 multipart | 206 single part (first range only) |
| `If-Range: <etag>` matching | **200 with full `Content-Length` but only the range bytes in the body** (GET has no ETag to compare) | no ETag support |
| `If-Range: <date>` matching | 206 correct | 206 |
| `If-Range: <date>` not matching | 200 with full `Content-Length`, **truncated body** | 206 (validator ignored) |
| `If-None-Match` | ignored | ignored |

AltMount root cause: `internal/utils/range.go` `ParseRangeHeader` returns `Start=-1` for suffix ranges, and the reader then finds "No segments to download". `metadata_remote_file.go` treats this as a streaming failure and, with the health system disabled, "marking corrupted without triggering repair". The file disappears from WebDAV, while SAB history still says `Completed`. The same happens for a range end past EOF. Both were reproduced deterministically on separate files.

**Upstream history (checked 2026-10-07): this is an accidental regression, already known, and only partly fixed.**

- 2025-08-10 (`244d1de`): `utils.FixRangeHeader(rh, size)` is added and called from `getRequestRange`. It converted suffix ranges to absolute ones and clamped the end to `size-1`, which covers exactly both failure cases. `RangeHeader.Decode(size)` (rclone-derived, also suffix-aware) exists alongside it.
- 2025-08-19 (`5450215`, "Refactor segment handling…"): the refactor replaced the `FixRangeHeader` call with raw `Start`/`End`. Its message says the intent was to "simplify range handling and ensure proper bounds checking". Nothing indicates a deliberate trade-off.
- 2026-06-04 (`cec37d8`, PR #659 "dead-code sweep"): the now-unused `FixRangeHeader` is deleted.
- 2026-07-18: **v0.3.2 is released with both bugs.** It is still the latest release; `main` is 114 commits ahead.
- 2026-09-08: **PR #937 merged** on `main`, by the maintainer. It clamps a range end past EOF and returns `io.EOF` for a start at/after EOF, with regression tests. It describes exactly our past-EOF symptom ("the healthy file then 404'd on WebDAV"). **Not released yet.**
- 2026-10-06: **PR #976 opened** by a contributor: "normalize suffix ranges, unsatisfiable ranges return 416". Still open, unreviewed, no comments. `main`'s `getRequestRange` still passes `Start=-1` for suffix ranges, so **the suffix-range bug is unfixed even on `main`**.
- Related, independent: issue #749 / PR #930 (merged 2026-09-06, unreleased). Transient article misses no longer condemn a file outright. This reduces, but does not remove, the "file permanently hidden" risk noted in §5.

Consequences: no upstream report is needed. A thumbs-up or review on #976 with our reproduction would help it land. Mountenant's own Range normalisation stays mandatory because (a) the pinned release has both bugs, (b) the suffix fix is not merged, and (c) NzbDav has its own suffix bug. Once a release contains #937 and #976, re-run the §4.4 matrix and re-pin.

**Required Mountenant behaviour (new spec rule for §10.2/§5.3 UC-21):**

1. Parse the client's `Range` and `If-Range` in Mountenant against the catalogued size and Mountenant's own ETag/Last-Modified (Go's `http.ServeContent` logic is the reference).
2. Forward to the backend **only** `Range: bytes=a-b` with `0 ≤ a ≤ b ≤ size-1`, or no `Range` at all. Never forward suffix, open-ended, multi-range, `If-Range`, `If-None-Match` or `If-Modified-Since`.
3. Answer unsatisfiable ranges with 416 in Mountenant without calling the backend. For multi-range requests, ignore `Range` and serve the full file with 200 (RFC 9110 permits this; ADR 0003).
4. Validate the upstream response: status 206 with the expected `Content-Range`, or 200 when no range was sent, and `Content-Length` = expected. Upstream errors are handled in two phases:
   - **Before the first byte** (upstream status line is a 5xx, a 404, or fails validation): nothing has gone to the client yet, so Mountenant may retry (5xx, §10.4). It then answers 502 `backend_unavailable`, or for a 404 answers 410 `content_missing` and fails the job. The upstream status code is never passed through.
   - **After the first byte** (upstream connection drops, body shorter than `Content-Length`, read timeout): Mountenant's status line and `Content-Length` are already sent, so it aborts the client connection (`panic(http.ErrAbortHandler)`). The browser then sees a short body and offers to resume with a new `Range` request, which goes through the same normalisation. A backend error never becomes a "successful" truncated file.
5. Copy only an allowlist of upstream headers. NzbDav sends `Set-Cookie: nzb-webdav-backend=…`, `Server: Kestrel` and `X-Powered-By: Express`, which must not reach browsers.

Client cancellation: aborting the client connection tore down AltMount's upstream stream immediately (`GET /api/files/active-streams` was empty within 3 s), so the context-propagation assumption in §10.4 holds.

Intermittent errors: during the first minutes NzbDav answered several full and range GETs with `500` (empty body, ~5–22 s), and the same requests succeeded shortly after. This was not reproducible later under parallel load. The likely cause is provider-connection contention (both backends were configured with 50 connections on the same account). `Open` must treat 5xx before the first byte as retryable (the existing §10.4 retry rule covers it).

### 4.5 `Delete(ref)`

| | AltMount | NzbDav |
| --- | --- | --- |
| SAB `mode=history&name=delete&value=<nzo>` | Removes the history row only. **`del_files` is ignored, content stays on WebDAV.** Always `{"status":true}`, also for unknown ids (idempotent) | Removes the history row. Standard `del_files` is ignored. **Non-standard `del_completed_files=1` removes the content.** Unknown/already deleted id → **HTTP 500** without `del_completed_files`; **200 with it** (re-tested in the spike, `docs/spike-backend-adapter.md` §4.3), so the profile's call is idempotent |
| WebDAV `DELETE` on the job directory | **204**, content gone; repeat → 204 (idempotent) | 403 while `webdav.enforce-readonly=true`; with `false`: 200, repeat → 404 |
| Queue (in-progress) | same `mode=history&name=delete` works for queue items; `mode=queue&name=delete` also exists | `mode=queue&name=delete&value=<nzo>` |

Profile procedures:

```sh
# AltMount: both steps, both idempotent
curl "$AM_API?mode=history&name=delete&value=$NZO&output=json&apikey=$K"
curl -u … -X DELETE "$AM_DAV/complete/mountenant/$JOB_ID/"            # 204

# NzbDav: delete, then verify; never infer success from the 500 itself
curl "$ND_API?mode=history&name=delete&value=$NZO&del_completed_files=1&output=json&apikey=$K"
```

NzbDav delete must not treat a 500 as "already gone". A database lock or disk error also returns 500, and the two cases can't be told apart from the response. Decide on state instead:

1. Send the delete. Its response only selects the next step; it does not decide success.
2. **Verify**: `mode=queue&nzo_ids=$NZO` and `mode=history&nzo_ids=$NZO` both return no slot, **and** `PROPFIND Depth: 0` on `/content/<cat>/<jobId>/` returns 404. Tested: for a deleted GUID, both lookups return `200` with an empty `slots` list (no 500), and PROPFIND returns 404. The 500s come only from the *delete* call itself.
3. Both true → delete complete. History row gone but directory still there → WebDAV `DELETE` (needs `enforce-readonly=false`), then verify again. Otherwise → retry with backoff. Keep the job in a "backend cleanup pending" state (the Deleted row stays, cleanup is retried by the reconciler), and alert after N failures.

The same verify step also suits AltMount, even though its calls are idempotent: a PROPFIND 404 on the job directory is the authoritative "gone" signal on both backends. The orphan sweep (§4.6) is the final safety net for anything a failed delete leaves behind.

### 4.6 `ListNamespace(namespace)` (orphan sweep, UC-16)

`PROPFIND Depth: 1` on the category directory lists one child per job: AltMount `/webdav/complete/mountenant/`, NzbDav `/content/mountenant/`. Child names are job IDs (or `<jobId> (2)` for NzbDav duplicates, which are always orphans). Deleting orphans:

- AltMount: WebDAV `DELETE` (works out of the box).
- NzbDav: WebDAV `DELETE` requires `webdav.enforce-readonly=false`. Items that still have a history row can be removed with `del_completed_files=1` instead.

The sweep should also list SAB history filtered by category and delete rows whose name is not a live job ID.

**Race safety.** The sweep cannot delete a job that is just being submitted, because of three rules that together close the race:

1. **Persist before submit.** UC-10 commits the job row (status Queued) *before* calling `Submit`. Every directory or history row named `<jobId>` therefore has a matching DB row at the moment the backend creates it. The sweep reads job IDs from the DB *after* listing the backend, so any job submitted before the listing is in the ID set.
2. **Match on any non-Deleted status.** Queued, Importing, Ready and Failed rows all protect their directory. A Failed job's content is removed by the expiry/delete path, not by the sweep.
3. **Minimum age.** Ignore entries whose `getlastmodified` (directory) or `completed_at` (history) is younger than a grace period, e.g. `importTimeout`. Backend clocks are skewed (§5, item 3), so compare the backend value with the backend's own `Date` header, not Mountenant's clock. Also skip anything whose name is not a Mountenant job name, i.e. a UUID optionally followed by NzbDav's duplicate suffix ` (n)`; this leaves operator-created items alone while still catching `<jobId> (2)` orphans.

Schedule: every `orphanSweepInterval` (spec default 6 h), and once 10 minutes after startup. Never run it concurrently with itself (single in-process ticker). Use `orphanSweepDryRun` for the first deployment.

### 4.7 Readiness (`/readyz`)

| | Call | Note |
| --- | --- | --- |
| AltMount | `GET $AM_API?mode=version&output=json&apikey=$K` → check `status:true` | Validates the key (wrong key → `status:false`, HTTP 200) |
| NzbDav | `GET $ND_API?mode=queue&output=json&apikey=$K` | `mode=version` needs no key, so it does not validate it |
| Both | `PROPFIND Depth: 0` on the category directory | Validates WebDAV credentials. On NzbDav the directory only exists after the first import into that category (404 before) |

### 4.8 Ready re-verification (UC-14)

A shallow `PROPFIND Depth: 1` on the job directory is cheap (milliseconds) on both. It is the only reliable signal: AltMount kept reporting `Completed` in SAB history for a job whose file had been marked corrupted and returned 404. Re-verification must also check that each catalogued file is still listed, not only the directory.

## 5. Other observations

1. **Isolation.** Path-traversal attempts (`../`, `%2e%2e`, `..%2f`) stayed inside each WebDAV namespace (AltMount 307/404, NzbDav 404). Wrong WebDAV credentials → 401 on both. Isolation remains Mountenant's job (catalogue-built paths, §7.5), so the single shared category loses nothing.
2. **URL encoding.** Job directories are UUIDs, but `RelPath`s come from the release and can contain spaces, `#`, `%`, `?` or non-ASCII. Build backend URLs by escaping each path segment (`url.PathEscape`), never by string concatenation.
3. **Clocks.** Both backends' `Date` / `Last-Modified` / `completed_at` values were about two hours ahead of real UTC (the containers report local wall-clock time as GMT). Mountenant must use only its own clock for `importTimeout`, retention and `ExpiresAt`, and treat backend timestamps as opaque validators.
4. **Corruption is sticky on AltMount.** With `health.enabled=false`, a file marked corrupted stays hidden; `POST /api/health/{id}/check-now` failed ("Failed to start background health check"). The job has to be re-imported. Mountenant's Ready → Failed `backend_content_missing` transition (§10.3) is therefore the correct behaviour. Range normalisation (§4.4) keeps Mountenant from causing this itself, but a file can still be marked corrupted for other reasons, e.g. articles removed from the provider after import.
   **Recovery is a fresh import, and it was tested.** After a Ready job fails with `backend_content_missing`, the user re-uploads the NZB. Failed jobs don't count as duplicates (spec §4.2), so this creates a **new jobId**, hence a new `nzbname`/nzo_id, a new directory `<category>/<newJobId>/`, and a new health record keyed by the new path. The corrupted directory is never reused. In the study, Debian imports `bacfc4ab…` and `aba09424…` were fresh jobs submitted after `2d9f4f94…` had been marked corrupted, and both served the file correctly. The old job's directory and health record are cleaned up by its delete/expiry path (WebDAV `DELETE` on AltMount; the leftover health row is harmless and can be removed with `DELETE /api/health/{id}`). If the articles are really gone from the provider, the new import fails at its segment check with a clear `fail_message` instead of becoming Ready. AltMount's health/repair system (off by default) was not evaluated; it relies on *arr integrations and does not fit Mountenant's model.
5. **Shared provider.** Running both backends against one provider account at full connection count is a test artefact. In production only one backend is active (§10.1), and the spec's assumption of one shared provider account holds.
6. **Duplicate history rows on AltMount.** The SAB history view deduplicates by NZB filename. Unique `<jobId>.nzb` filenames avoid all cross-job interference.

## 6. Required specification changes

Applied in `docs/specification.md` revision 2 (2026-10-07); the decisions are recorded in `docs/adr/` 0002–0005.

| Section | Change |
| --- | --- |
| §2 Namespace, §7.5, §10.2 `Submit` | Replace per-user category `mt-<userId>` with **one configured category** (`backend.category`, default `mountenant`) and **one directory per job named `<jobId>`**. Submit with multipart filename `<jobId>.nzb` and `nzbname=<jobId>`. Isolation stays catalogue-based. |
| UC-16 | Sweep lists `<category>/` (Depth 1) and SAB history by category; anything not a non-Deleted job ID (including `<jobId> (n)`) is an orphan. Race safety: persist-before-submit, read DB IDs after listing, and a minimum-age grace period (§4.6). |
| §7.6 | Accept the standard NZB `<!DOCTYPE nzb PUBLIC …>`; reject only internal DTD subsets and entity declarations (Go `encoding/xml` never resolves entities). |
| UC-10 | Decompress `.nzb.gz` in Mountenant as a bounded stream (limit = `maxNzbBytes`) and always submit plain XML. |
| §10.2 `Open`, UC-21 | Add the Range normalisation rules of §4.4 (Mountenant evaluates `Range`/`If-Range` itself, forwards only closed in-range single ranges, validates the upstream response, allowlists headers). Mountenant owns `ETag` and `Content-Type`. |
| §10.2 `Delete` | Profile-specific: AltMount = history delete + WebDAV `DELETE`; NzbDav = history delete with `del_completed_files=1` (with that parameter an unknown id → 200; see the spike). Success is decided by verification (history lookup + PROPFIND 404), never by the response code; failed cleanups are retried by the reconciler (§4.5). |
| §10.2 `Open` | Upstream errors before the first byte → retry, then 502/410; after the first byte → abort the client connection (§4.4). |
| §10.2 `Status`, §10.3 | Use the status table of §4.2. Treat AltMount history `Unknown` as "not finished". Always check the SAB `status` field (AltMount errors are HTTP 200). |
| §10.2 `ListFiles` | Parse `href` as URL, use only the path; directories via `resourcetype`; NzbDav root is `/content/`. |
| §10.4 | Before re-submitting, search queue and history for `name == jobId`. |
| §10.5 | Add the required backend settings from §3 as documented operator prerequisites. They cannot be verified through the SAB API or WebDAV alone, so a startup self-check would need the products' private config APIs; v1 should document them instead. A contract test should cover each misconfiguration's `fail_message`. |
| §15.1 Risks | Add: "Compressed RAR releases cannot be served (both backends)" → surface the backend `fail_message` to the user; "AltMount Range parsing can permanently hide files" → mitigated by Range normalisation; upstream: past-EOF fixed on main (#937, unreleased), suffix fix pending (#976). |
| §15.2 Backend versions | Pin AltMount 0.3.2 (`14ad0f5`) and NzbDav 0.6.4 for v1 contract tests. |

## 7. Profile summary (input for `internal/jobs/adapters/sabdav`)

| Profile field | `altmount` | `nzbdav` |
| --- | --- | --- |
| `apiPath` | `/sabnzbd/api` | `/api` |
| `davRoot` | `/webdav` | `` (root) |
| `jobDir(cat, jobId)` | `/webdav/complete/{cat}/{jobId}/` | `/content/{cat}/{jobId}/` |
| `nzoID` | = `jobId` (from `nzbname`) | from submit response |
| `categoryMustExist` | yes (operator config) | no |
| `submitFileField` | `name` | `name` |
| `statusMap` | Queued/Paused→Queued, Downloading→Importing, history Unknown→ignore, Completed→verify, Failed→Failed | Queued→Queued, Downloading→Importing, Completed→verify, Failed→Failed |
| `apiErrorStyle` | HTTP 200 + `status:false` | 401 / 500 + `status:false` |
| `delete` | history delete + WebDAV `DELETE` dir; then verify | history delete `&del_completed_files=1`; then verify (never infer success from a 500, §4.5) |
| `hrefStyle` | path, trailing `/` on dirs | absolute URL, no trailing `/` |
| `etag` / `contentType` | ignore / ignore (Mountenant computes) | none / octet-stream (Mountenant computes) |
| `readyProbe` | `mode=version` + PROPFIND category | `mode=queue` + PROPFIND category |
| Range quirks | never send suffix or past-EOF ranges (destroys file) | never send suffix ranges (wrong bytes) |

## 8. Appendix — reproducible commands

```sh
export AM=http://<altmount-host>:8080  AMK=<altmount api key>  AMDAV=usenet:<pass>
export ND=http://<nzbdav-host>:3000  NDK=<nzbdav api key>    NDDAV=admin:<pass>
J=$(uuidgen | tr A-Z a-z)

# submit
curl -X POST "$AM/sabnzbd/api?mode=addfile&output=json&apikey=$AMK&cat=mountenant&nzbname=$J" -F "name=@Debian.10.Netinstall.iso.nzb;filename=$J.nzb"
curl -X POST "$ND/api?mode=addfile&output=json&apikey=$NDK&cat=mountenant" -F "name=@Debian.10.Netinstall.iso.nzb;filename=$J.nzb"

# status
curl "$AM/sabnzbd/api?mode=history&output=json&apikey=$AMK&nzo_ids=$J"
curl "$ND/api?mode=history&output=json&apikey=$NDK&category=mountenant"

# list
curl -u $AMDAV -X PROPFIND -H 'Depth: infinity' "$AM/webdav/complete/mountenant/$J/"
curl -u $NDDAV -X PROPFIND -H 'Depth: infinity' "$ND/content/mountenant/$J/"

# read (closed range only)
curl -u $AMDAV -r 0-1048575 -o part.bin "$AM/webdav/complete/mountenant/$J/debian-10.0.0-arm64-netinst.iso"
curl -u $NDDAV -r 0-1048575 -o part.bin "$ND/content/mountenant/$J/debian-10.0.0-arm64-netinst.iso"

# delete
curl "$AM/sabnzbd/api?mode=history&name=delete&value=$J&output=json&apikey=$AMK"; curl -u $AMDAV -X DELETE "$AM/webdav/complete/mountenant/$J/"
curl "$ND/api?mode=history&name=delete&value=<nzo_id>&del_completed_files=1&output=json&apikey=$NDK"

# DO NOT run against AltMount data you want to keep — demonstrates the corruption bug
# curl -u $AMDAV -H 'Range: bytes=-1024' "$AM/webdav/complete/mountenant/$J/<file>"
```

State after the study: all test jobs, history rows, WebDAV content and AltMount health records were removed from both instances. The configuration changes in §3 are left in place. The empty category directory `mountenant` exists on both.
