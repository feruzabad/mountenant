# 6. Transparent upstream resume

Date: 2026-10-07

## Status

Accepted

Supersedes the "errors after the first byte" part of ADR-0003 decision 5. The
rest of ADR-0003 stands.

## Context

ADR-0003 says the streaming proxy aborts the client connection when the backend
body breaks after the first byte. The client then sees a short body and is
expected to resume with a new `Range` request.

The backend adapter spike (`docs/spike-backend-adapter.md` §4.1) found that
AltMount 0.3.2 often ends full downloads early. It answers `200` with the full
`Content-Length`, then closes the connection part-way through, and logs nothing
at info level. Plain curl reproduced this in 2 of 9 full downloads of a 262 MB
file, with no Mountenant code involved. Through the proxy, one in four
downloads failed in the first live run. A follow-up run saw 3 cut-offs in 8
downloads. The file itself is not damaged: the next request for the same bytes
succeeds. The likely cause is article fetch errors that AltMount turns into
end-of-file instead of retrying.

With ADR-0003's behaviour, every cut-off becomes a failed download for the
User. Browsers don't reliably resume on their own (Chrome shows
"Failed - Network error" and waits for a click), and download managers resume
at best. For large files, a cut-off becomes close to certain.

Alternatives considered:

- *Abort and rely on client resume* (ADR-0003 as written): correct, but users
  see failures that Mountenant could hide.
- *Buffer or spool to disk and retry*: violates G3 (no local disk for content).
- *Retry the whole response from the start*: impossible once bytes are on the
  wire.

## Decision

When the backend body ends or fails before the promised length has been sent,
and the client is still connected, the proxy reopens the backend for exactly the
remaining bytes, `bytes=<first+sent>-<last>`, and keeps writing into the same
response. This happens at most `MaxResumes` times per response (default 3).
Each reopen is a normal `Backend.Open` with a closed in-file range, so
ADR-0003's normalisation and response checks apply to it as well.

Only when resumes are used up, or a reopen fails, does the proxy reset the
client connection (`http.ErrAbortHandler`), as ADR-0003 describes. Client write
errors are never resumed; they mean the client is gone.

Every resume is logged with job, path, offset and attempt, and counted
(`Proxy.Resumes()`, to be exported as a metric).

## Consequences

- AltMount's cut-offs are invisible to Users. In the spike's live run, 3/3
  cut-offs were resumed and all 8 downloads matched the reference SHA-256.
- A response can be stitched from several backend streams. That is only correct
  because job content is immutable; Mountenant never serves a job whose content
  can change.
- A failing backend can cost up to `MaxResumes` extra requests per download
  before the client sees an error. The bound keeps that small, and retries of
  `Open` itself stay with the adapter (§10.4).
- The resume counter shows backend health. A rising rate is an early warning for
  provider or backend problems.
- The ZIP endpoint (specification §14) streams files one after another through
  the same `Open` calls and should reuse this mechanism per entry.

References: `docs/spike-backend-adapter.md` §1, §3.2, §4.1;
`docs/adr/0003-mountenant-owns-http-range-semantics.md`; `docs/specification.md`
§1.1 G3, §10.4, §12.3, §14.
