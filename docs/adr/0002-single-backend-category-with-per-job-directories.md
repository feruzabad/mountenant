# 2. Single backend category with per-job directories

Date: 2026-10-07

## Status

Accepted

## Context

The phase-1 specification gives every User a Namespace `mt-<userId>` and uses
it as the SABnzbd category on `Submit` (§2, §7.5, §10.2). The orphan sweep
(UC-16) lists all `mt-*` namespaces.

The feasibility study found this does not work on AltMount 0.3.2:

- `mode=addfile` with a category that is not pre-configured fails with
  `invalid category '…' - not found in configuration`. Categories are part of
  AltMount's `sabnzbd.categories` config section.
- The only way to add a category at runtime is AltMount's private config API
  (`/api/auth/login` with UI credentials, then `PATCH /api/config/sabnzbd` with
  the full section). This is not part of the SABnzbd API or WebDAV.

NzbDav 0.6.4 accepts any category string and creates the directory on demand,
so the per-user design would only have worked there.

Further observations from the study that shape the replacement:

- On both backends the Job directory name is the multipart **filename** without
  `.nzb`. AltMount uses `nzbname` as the nzo_id; NzbDav ignores `nzbname` and
  returns a random GUID.
- Without a controlled filename, the directory takes the NZB's `<meta name>` or
  original filename, and duplicates merge into one directory (AltMount) or get
  a `(2)` suffix (NzbDav).
- Path-traversal attempts stayed inside each WebDAV namespace, and isolation
  already rests on catalogue-built paths (§7.5), not on the category.

Rejected alternative: create `mt-<userId>` categories through AltMount's config
API on user sync. It couples Mountenant to an unstable, product-specific API
(against §3.1 "SABnzbd API + WebDAV standards"), needs UI credentials and a
rate-limited login, requires sending the full `sabnzbd` section on each change,
and has no NzbDav equivalent to keep the profiles symmetric.

## Decision

- Mountenant uses **one configured backend category** for all Jobs:
  `backend.category`, default `mountenant`. The operator adds it to the
  backend's category list (required on AltMount, not needed on NzbDav).
- Each Job gets **one directory named `<jobId>`** inside that category.
  `Submit` always sends the multipart filename `<jobId>.nzb` and
  `nzbname=<jobId>`. The Job directory is then `<category>/<jobId>/` on both
  backends (AltMount: `/webdav/complete/<cat>/<jobId>/`, NzbDav:
  `/content/<cat>/<jobId>/`).
- `BackendRef` stores the returned `nzo_ids[0]` (equal to `jobId` on AltMount,
  a GUID on NzbDav) and the Job directory path.
- Tenant isolation remains catalogue-based (§7.5): every backend path is built
  server-side from category, `jobId` and a catalogued `RelPath`; every job query
  is scoped by `owner_id`. The category is not a security boundary.
- `ListNamespace` lists the single category directory (`PROPFIND Depth: 1`) and
  SAB history filtered by category. Child names are Job IDs; anything that is
  not a non-Deleted Job ID, including `<jobId> (n)`, is an orphan (see ADR-0004
  for the sweep's race-safety rules).
- Before re-submitting (§10.4), the adapter searches queue and history for
  `name == jobId` to avoid duplicate directories.

## Consequences

- No product-specific config API is needed at runtime; the category is a
  one-time operator prerequisite (documented with the §3.1 checklist of the
  study).
- Backend items are traceable to a Job by name alone; unique `<jobId>.nzb`
  filenames also avoid AltMount's history deduplication by filename.
- The term Namespace changes meaning: it is no longer per User but the single
  Mountenant category. The specification's glossary, §7.5, §10.2, UC-16, §15.1
  and §15.2 ("each user's job is imported separately in their own namespace")
  must be updated, and the `Backend` port signatures `Submit(nzb, namespace)` /
  `ListNamespace(namespace)` revisited.
- Per-user usage can no longer be read from the backend by category; it comes
  from Mountenant's own database, which is already the source of truth.
- A misconfigured category on AltMount fails every submit; `/readyz` probes the
  category directory and contract tests cover the error message.

References: `docs/feasibility.md` §1 (#1), §3.1, §4.1, §4.6, §5 (items 1, 6),
§6; `docs/specification.md` §2, §4.5, §7.5, §10.2, UC-16, §15.2.
