# 1. Record architecture decisions

Date: 2026-10-07

## Status

Accepted

## Context

Mountenant is specified in `docs/specification.md`. The specification states the
design as it currently stands, but not how it got there: when a section changes,
the reason for the change and the alternatives that were rejected are lost.

The phase-2 backend case study (`docs/feasibility.md`) already forced several
changes to the phase-1 specification. Those changes rest on observed backend
behaviour (AltMount 0.3.2, NzbDav 0.6.4) that is not obvious from the
specification text alone, and future contributors need to know why the design
looks the way it does before they "simplify" it back.

Specification §3.1 lists a handful of key decisions in a table and says that
architecture decisions are recorded as ADRs in Michael Nygard format in
`docs/adr/`.

## Decision

We record architecturally significant decisions as Architecture Decision
Records, following Michael Nygard's format
("Documenting Architecture Decisions", 2011):

- One Markdown file per decision in `docs/adr/`, named
  `NNNN-short-title-in-kebab-case.md`, numbered sequentially and never reused.
- Sections: Title, Date, Status, Context, Decision, Consequences, plus a
  References line pointing at the relevant specification and study sections.
- Status is one of Proposed, Accepted, Deprecated, or Superseded by ADR-NNNN.
- An accepted ADR is not rewritten. A changed decision gets a new ADR, and the
  old one is marked Superseded with a link to its successor. Typos and broken
  links may be fixed in place.
- `docs/adr/README.md` lists all ADRs with their current status.

The specification remains the description of the current design. When an ADR
is accepted, the affected specification sections are updated to match, and the
ADR is the place that explains why.

## Consequences

- Decisions and their rejected alternatives are written down once, close to the
  code, and reviewed like any other change.
- The specification can stay concise; rationale lives in the ADRs.
- There is a small overhead per decision. Only decisions that constrain the
  architecture, the backend contract or security get an ADR; routine
  implementation choices do not.
- Specification and ADRs can drift. A reviewer of a specification change checks
  whether an ADR needs to be added or superseded.

References: `docs/specification.md` §3.1; `docs/feasibility.md` §1, §6.
