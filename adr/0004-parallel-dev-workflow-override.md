# ADR-0004 — Parallel-dev workflow override

## Status

Accepted — 2026-05-17

## Context

CLAUDE.md declares: "Push directly to main. No branches, no MRs." Parallel-dev fundamentally requires branches per worker + 1 MR per feature.

## Decision

Override "push to main" rule for parallel-dev waves ONLY:
1. Planner creates synthetic YT epic + N child tasks
2. Distribute allocates N worktrees on `parallel-dev/<feature_id>/<task_id>` branches
3. Workers commit to their own branches (local-only, never pushed)
4. Merge-coordinator creates `merge/<feature_id>` from origin/main, applies worker patches, runs lint+test, opens ONE MR
5. Auto-delete branch on merge (no long-lived branches)

Direct human commits continue push-to-main as before.

## Consequences

Positive: parallel-dev becomes available + auditable via the single MR. Human velocity unchanged.

Negative: TWO workflows exist; documented in `steering/repo-conventions.md`.

## Alternatives

- Deny parallel-dev (rejected — Hub is too big a surface to leave unsupported)
- Convert Hub to MR-only (rejected — disruptive to 2-contributor velocity)
