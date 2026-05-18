# Threat Model — Requirements — SMS + Email routing destinations (spec/009) (spec/009-sms-email-routing-destinations)

## Context

- Repo: `meshsat-hub` (security_posture: internet-exposed)
- Spec: `spec/009-sms-email-routing-destinations`
- Source REQ anchors (top 5):
  - `REQ-800: The system shall provide capability 1 of sms + email routing destinations per `meshsat-hub/docs/ROADMAP.md` (MESHSAT-181).`
  - `REQ-801: The system shall provide capability 2 of sms + email routing destinations per `meshsat-hub/docs/ROADMAP.md` (MESHSAT-181).`
  - `REQ-802: The system shall provide capability 3 of sms + email routing destinations per `meshsat-hub/docs/ROADMAP.md` (MESHSAT-181).`
  - `REQ-803: The system shall provide capability 4 of sms + email routing destinations per `meshsat-hub/docs/ROADMAP.md` (MESHSAT-181).`
  - `REQ-804: The system shall provide capability 5 of sms + email routing destinations per `meshsat-hub/docs/ROADMAP.md` (MESHSAT-181).`

## Trust boundaries

- Internet → public API (TLS termination, rate limiting, WAF if present)
- Public API → internal service mesh (mTLS or service-token)
- Service → datastore (per-tenant isolation, encryption at rest)
- Tenant A ↮ Tenant B (every query MUST filter by tenant_id)

## Spoofing

_Adversary forges identity (user, service, or peer)._

### Threats specific to this spec

| # | Threat | Asset | Mitigation | Status |
|---|---|---|---|---|
| S1 | [NEEDS_REVIEW] Concrete threat #1 derived from REQ-001 et al. | <data/api/identity> | <auth/crypto/rate-limit/audit/...> | UNAUDITED |
| S2 | [NEEDS_REVIEW] Concrete threat #2 | <asset> | <mitigation> | UNAUDITED |
| S3 | [NEEDS_REVIEW] Concrete threat #3 | <asset> | <mitigation> | UNAUDITED |

### Mitigations already in place

- [NEEDS_REVIEW] List the existing controls (from constitution + requirements) that already address Spoofing.

### Open questions

- [NEEDS_REVIEW] What does a successful spoofing attack look like end-to-end here?
- [NEEDS_REVIEW] Have we tested the spoofing surface (red-team / fuzz / abuse case)?

## Tampering

_Adversary modifies data in transit or at rest._

### Threats specific to this spec

| # | Threat | Asset | Mitigation | Status |
|---|---|---|---|---|
| T1 | [NEEDS_REVIEW] Concrete threat #1 derived from REQ-001 et al. | <data/api/identity> | <auth/crypto/rate-limit/audit/...> | UNAUDITED |
| T2 | [NEEDS_REVIEW] Concrete threat #2 | <asset> | <mitigation> | UNAUDITED |
| T3 | [NEEDS_REVIEW] Concrete threat #3 | <asset> | <mitigation> | UNAUDITED |

### Mitigations already in place

- [NEEDS_REVIEW] List the existing controls (from constitution + requirements) that already address Tampering.

### Open questions

- [NEEDS_REVIEW] What does a successful tampering attack look like end-to-end here?
- [NEEDS_REVIEW] Have we tested the tampering surface (red-team / fuzz / abuse case)?

## Repudiation

_Adversary denies having performed an action with no audit trail._

### Threats specific to this spec

| # | Threat | Asset | Mitigation | Status |
|---|---|---|---|---|
| R1 | [NEEDS_REVIEW] Concrete threat #1 derived from REQ-001 et al. | <data/api/identity> | <auth/crypto/rate-limit/audit/...> | UNAUDITED |
| R2 | [NEEDS_REVIEW] Concrete threat #2 | <asset> | <mitigation> | UNAUDITED |
| R3 | [NEEDS_REVIEW] Concrete threat #3 | <asset> | <mitigation> | UNAUDITED |

### Mitigations already in place

- [NEEDS_REVIEW] List the existing controls (from constitution + requirements) that already address Repudiation.

### Open questions

- [NEEDS_REVIEW] What does a successful repudiation attack look like end-to-end here?
- [NEEDS_REVIEW] Have we tested the repudiation surface (red-team / fuzz / abuse case)?

## Information Disclosure

_Adversary reads data they should not be able to read._

### Threats specific to this spec

| # | Threat | Asset | Mitigation | Status |
|---|---|---|---|---|
| I1 | [NEEDS_REVIEW] Concrete threat #1 derived from REQ-001 et al. | <data/api/identity> | <auth/crypto/rate-limit/audit/...> | UNAUDITED |
| I2 | [NEEDS_REVIEW] Concrete threat #2 | <asset> | <mitigation> | UNAUDITED |
| I3 | [NEEDS_REVIEW] Concrete threat #3 | <asset> | <mitigation> | UNAUDITED |

### Mitigations already in place

- [NEEDS_REVIEW] List the existing controls (from constitution + requirements) that already address Information Disclosure.

### Open questions

- [NEEDS_REVIEW] What does a successful information disclosure attack look like end-to-end here?
- [NEEDS_REVIEW] Have we tested the information disclosure surface (red-team / fuzz / abuse case)?

## Denial of Service

_Adversary makes the service unavailable to legitimate users._

### Threats specific to this spec

| # | Threat | Asset | Mitigation | Status |
|---|---|---|---|---|
| D1 | [NEEDS_REVIEW] Concrete threat #1 derived from REQ-001 et al. | <data/api/identity> | <auth/crypto/rate-limit/audit/...> | UNAUDITED |
| D2 | [NEEDS_REVIEW] Concrete threat #2 | <asset> | <mitigation> | UNAUDITED |
| D3 | [NEEDS_REVIEW] Concrete threat #3 | <asset> | <mitigation> | UNAUDITED |

### Mitigations already in place

- [NEEDS_REVIEW] List the existing controls (from constitution + requirements) that already address Denial of Service.

### Open questions

- [NEEDS_REVIEW] What does a successful denial of service attack look like end-to-end here?
- [NEEDS_REVIEW] Have we tested the denial of service surface (red-team / fuzz / abuse case)?

## Elevation of Privilege

_Adversary gains capabilities beyond their authorisation level._

### Threats specific to this spec

| # | Threat | Asset | Mitigation | Status |
|---|---|---|---|---|
| E1 | [NEEDS_REVIEW] Concrete threat #1 derived from REQ-001 et al. | <data/api/identity> | <auth/crypto/rate-limit/audit/...> | UNAUDITED |
| E2 | [NEEDS_REVIEW] Concrete threat #2 | <asset> | <mitigation> | UNAUDITED |
| E3 | [NEEDS_REVIEW] Concrete threat #3 | <asset> | <mitigation> | UNAUDITED |

### Mitigations already in place

- [NEEDS_REVIEW] List the existing controls (from constitution + requirements) that already address Elevation of Privilege.

### Open questions

- [NEEDS_REVIEW] What does a successful elevation of privilege attack look like end-to-end here?
- [NEEDS_REVIEW] Have we tested the elevation of privilege surface (red-team / fuzz / abuse case)?

## Review log

| Date | Reviewer | Action | Notes |
|---|---|---|---|
| 2026-05-18 | sdd-add-stride-threat-model.py | Generated skeleton | All threats marked [NEEDS_REVIEW] — security pass required before any spec moves to 'in-progress' |

## Owasp ASVS / MITRE ATT&CK alignment

- [NEEDS_REVIEW] Map each threat row to the relevant OWASP ASVS L2 control + MITRE ATT&CK technique ID (so we can update `openclaw/skills/security-triage/mitre-mapping.json`).
