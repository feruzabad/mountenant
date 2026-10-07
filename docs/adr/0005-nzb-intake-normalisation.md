# 5. NZB intake normalisation

Date: 2026-10-07

## Status

Accepted

## Context

UC-10 accepts `.nzb` and `.nzb.gz` uploads. Specification §7.6 parses NZBs with
Go's `encoding/xml`, decompresses `.nzb.gz` with a size limit, and rejects
DOCTYPE declarations. The feasibility study showed two problems with this as
written:

- **DOCTYPE.** Both real-world test NZBs (Debian, CentOS) carry the standard
  newzbin declaration `<!DOCTYPE nzb PUBLIC …>`. Rejecting every DOCTYPE would
  reject ordinary NZBs. Go's `encoding/xml` never resolves external or internal
  entities, so the declaration itself is not an XXE risk; the risk worth
  guarding against is an internal DTD subset with entity declarations.
- **Compressed NZBs.** Neither backend accepts `.nzb.gz`. AltMount accepts the
  upload and then fails with `XML syntax error … U+001F`; NzbDav rejects it with
  `'\u001F' … invalid character`. Forwarding the upload as received therefore
  fails on both.

Mountenant already parses the XML for validation (at least one file and one
segment), so it holds a decompressed view of the NZB anyway. The `nzb_blobs`
table keeps the NZB until the backend accepts it, and goal G3 ("no local
storage") concerns content bytes, not NZBs.

Rejected alternatives: keep rejecting all DOCTYPEs (rejects real NZBs);
forward `.nzb.gz` and rely on the backend (fails on both); decompress fully
into memory before checking size (unbounded memory for a high compression
ratio).

## Decision

1. **DOCTYPE.** Accept the standard NZB `<!DOCTYPE nzb PUBLIC …>`. Reject a
   DOCTYPE that contains an internal subset (`[ … ]`) or any `<!ENTITY`
   declaration with 400 `invalid_nzb`.
2. **Decompression.** A `.nzb.gz` upload (by gzip magic bytes, not only by
   name) is decompressed in Mountenant as a bounded stream:
   `io.LimitReader(gzip.NewReader(body), maxNzbBytes+1)`. Reaching the limit
   rejects the upload with 413 `nzb_too_large`. The limit is the owner's
   `MaxNZBBytes`, capped by `jobs.maxNzbBytesHardCap`.
3. **Submission.** Mountenant always submits plain XML to the backend, as
   multipart filename `<jobId>.nzb` (ADR-0002). The decompressed NZB is what is
   stored in `nzb_blobs` for retries.

## Consequences

- Real-world NZBs are accepted, and the XXE guard targets the actual risk.
- Memory per upload is bounded by the NZB quota regardless of the compression
  ratio (zip-bomb guard).
- Backends only ever see plain XML, so the backend profiles need no
  compression handling.
- The size limit applies to the decompressed NZB. The specification should say
  so explicitly in UC-10 and §7.6, and state whether `NZBDigest` is computed
  over the uploaded bytes (§4.2 today) or the decompressed XML; with the former,
  the same NZB uploaded once plain and once gzipped is not detected as a
  duplicate.

References: `docs/feasibility.md` §1 (#4 and hard limits), §2 (test data),
§4.1, §6; `docs/specification.md` §1.1 G3, §4.2, UC-10, UC-17, §7.6, §9.3.
