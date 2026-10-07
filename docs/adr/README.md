# Architecture Decision Records

This directory records Mountenant's architecturally significant decisions as
ADRs in Michael Nygard's format (see [ADR-0001](0001-record-architecture-decisions.md)).

- One file per decision, `NNNN-short-title.md`, numbered sequentially.
- Sections: Title, Date, Status, Context, Decision, Consequences, References.
- Status: Proposed, Accepted, Deprecated, or Superseded by ADR-NNNN.
- Accepted ADRs are not rewritten; a changed decision gets a new ADR that
  supersedes the old one.
- `docs/specification.md` describes the current design; ADRs explain why.

## Index

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | Accepted |
| [0002](0002-single-backend-category-with-per-job-directories.md) | Single backend category with per-job directories | Accepted |
| [0003](0003-mountenant-owns-http-range-semantics.md) | Mountenant owns HTTP range semantics | Accepted; mid-stream error handling superseded by 0006 |
| [0004](0004-verify-based-backend-deletion.md) | Verify-based backend deletion | Accepted |
| [0005](0005-nzb-intake-normalisation.md) | NZB intake normalisation | Accepted |
| [0006](0006-transparent-upstream-resume.md) | Transparent upstream resume | Accepted |
| [0007](0007-sqlite-storage-conventions.md) | SQLite storage conventions | Accepted |

ADRs 0002–0005 follow from the phase-2 backend case study
(`docs/feasibility.md`, AltMount 0.3.2 and NzbDav 0.6.4); ADR 0006 follows
from the backend adapter spike (`docs/spike-backend-adapter.md`).
