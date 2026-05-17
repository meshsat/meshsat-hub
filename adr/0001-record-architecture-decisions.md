# ADR-0001 — Record architecture decisions

## Status

Accepted — 2026-05-17

## Context

MeshSat Hub has accumulated significant tribal knowledge in CLAUDE.md (706 lines, gitignored) and the 3 incident postmortems (Incidents 11/13/14). Operator-only context can't be auto-loaded by parallel-dev workers or future contributors.

## Decision

MADR format. One ADR per file under `adr/NNNN-<title>.md`. Each ADR has Status, Context, Decision, Consequences, Alternatives considered.

## Consequences

Positive: every "would argue about this in 6 months" decision becomes searchable + versioned. Workers can read the ADR before making changes that violate the decision. ADRs from postmortems (e.g. "why we don't bootstrap with bare `gcomm://`") become permanent guardrails.

Negative: small discipline cost.

## Alternatives

- No ADRs: rejected — postmortem knowledge silently decays.
- Single ARCHITECTURE.md: rejected — incidents need their own dated context.
