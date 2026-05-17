# ADR-0002 — Security is #1 priority, above features

## Status

Accepted — 2026-05-17 (codifies CLAUDE.md L27-37)

## Context

MeshSat Hub is internet-exposed software. It accepts inbound HTTP webhooks from satellite ground stations, serves a public-facing web dashboard, and manages field devices in safety-critical environments (SOS, dead man's switch).

A typical product team treats security as a feature-equivalent priority: "ship the feature, fix the security issue in the next sprint." For a Hub that holds tenant data + signs SOS messages + bridges satellite credit, that posture is wrong.

## Decision

**Security is #1, above features, performance, and convenience.** When in conflict:
- A secure system missing a feature ships over an insecure system with a feature.
- A secure system that's 20% slower ships over a faster system with a CVE.
- A secure system that's inconvenient to deploy ships over a convenient system with an auth bypass.

Specific implications:
1. Every new HTTP handler requires auth review (no "I'll add it later")
2. Every new third-party dep requires security ADR (license + CVE count + maintenance signal)
3. HIGH-severity gosec/govulncheck findings BLOCK merge — they don't get filed for later
4. Public-URL OWASP ZAP scan gates every v0.X release
5. Tenant isolation gets THREE redundant layers (storage + transport + identity), not one

## Consequences

**Positive:**
- Multi-tenant trust holds. A tenant data leak is the catastrophic outcome we're optimising to avoid.
- Audit + compliance posture is shippable on demand.
- New developers internalise the priority via this ADR + Constitution Article II.

**Negative:**
- Feature velocity is slower than a feature-first competitor.
- Some refactors blocked or delayed by security review.
- This MUST be a written policy (this ADR) because verbal "security matters" loses to a roadmap deadline. Written ADR is the tiebreaker.

## Alternatives considered

- **Security is "important but not #1"** (rejected — invariably loses to feature deadlines)
- **Security through obscurity** (rejected — Hub is internet-exposed; not viable)
- **Trust-the-cloud-provider** (rejected — self-hosted SaaS, no AWS/GCP shield)

## References

- Constitution Article II
- `docs/SECURITY_AUDIT.md`
- `docs/ENCRYPTION.md`
- 3 incident postmortems: `docs/INCIDENT_{11,13,14}_POSTMORTEM.md`
