# Threat Model — SOS trigger (spec/001-sos-trigger)

## Context

- Repo: `meshsat-hub` (security_posture: internet-exposed)
- Spec: `spec/001-sos-trigger`
- Critical: **life-safety surface** — any compromise can cause real-world harm or mask a real distress signal.
- Source REQ anchors:
  - `REQ-001: The system shall subscribe to MQTT topic pattern meshsat/+/mo/decoded via the internal bus.`
  - `REQ-002: When a decoded mobile-originated message arrives, the system shall search the text field for SOS keywords case-insensitively.`
  - `REQ-003: The system shall recognise the keywords SOS, MAYDAY, EMERGENCY (defined in sosKeywords slice in internal/sos/detector.go).`
  - `REQ-005: When an SOS condition is detected, the system shall publish an SOSEvent JSON message to meshsat/{IMEI}/sos on the bus.`
  - `REQ-007: The system shall fire the escalation.Engine for the matched tenant_id with the SOSEvent payload.`

## Trust boundaries

- Satellite carrier (Cloudloop / Rock7 / Globalstar) -> Hub webhook receiver -- Cloudloop has **NO HMAC** (carve-out per Article IX); guarded by IP allowlist (`35.178.100.117, 52.56.155.169`) only.
- Hub webhook receiver -> internal bus (`meshsat/+/mo/decoded`) -- same process, in-memory.
- Internal bus -> SOS detector -- same process; trust assumed at goroutine level.
- SOS detector -> escalation engine -> outbound channels (SMS via Twilio, email via SMTP, push via ntfy) -- **adversary-reachable side channels** with their own auth.

## Spoofing

_Adversary forges identity (user, service, or peer)._

### Threats specific to this spec

| # | Threat | Asset | Mitigation | Status |
|---|---|---|---|---|
| S1 | Attacker IP-spoofs a Cloudloop webhook from a non-allowlisted source. The Cloudloop carve-out gives 35.178.100.117 + 52.56.155.169 a pass with no HMAC; if cloudfront/CDN trust-chain leaks the X-Forwarded-For evaluation can be bypassed. | Webhook ingress | Allowlist MUST be checked against `RemoteAddr` AFTER trusted-proxy resolution (NEVER against raw `X-Forwarded-For`). | UNAUDITED |
| S2 | Attacker uploads a CSV / config import that contains hostile IMEI mapped to attacker's tenant -- subsequent legitimate SOS from real IMEI gets routed to attacker. | IMEI -> tenant mapping | Reject IMEI rebinding unless `operator_id` matches the existing mapping's `operator_id`; require 2-step confirmation on rebind. | UNAUDITED |
| S3 | Attacker spoofs an internal-bus publisher (if the bus is ever exposed beyond the same process -- e.g. external MQTT broker mode). | Internal bus auth | While bus is in-process today, ANY future move to external broker MUST require mTLS client cert + topic-level ACL. Document in HUB_MODE switch. | UNAUDITED |

### Mitigations already in place

- Cloudloop IP allowlist (`DefaultCloudloopWebhookIPs = "35.178.100.117,52.56.155.169"` in `internal/webhook/cloudloop.go`).
- HMAC verification on every other carrier (Rock7, Globalstar) via `crypto/hmac.Equal()` constant-time compare.
- Per-tenant API tokens with scope; SOS write requires `scope:sos.write`.

### Open questions

- Has the Cloudloop allowlist been validated against the published Cloudloop outbound IP list in the past 30 days? (Out-of-band drift catches missed IP rotations.)
- Does the trusted-proxy chain on the public ingress (k8s ingress + service mesh sidecar) correctly preserve real client IP all the way to `internal/webhook/cloudloop.go:HandleCloudloop`?

## Tampering

_Adversary modifies data in transit or at rest._

### Threats specific to this spec

| # | Threat | Asset | Mitigation | Status |
|---|---|---|---|---|
| T1 | MITM modifies an in-flight SOS payload to flip the keyword (e.g. "SOS" -> "TEST") suppressing escalation. | Webhook body integrity | TLS 1.3 mandatory; HMAC on every non-Cloudloop carrier; Cloudloop traffic is over TLS-pinned link to the Hub -- confirm cert pin lives where the test asserts. | UNAUDITED |
| T2 | Insider with DB write access edits `sosKeywords` or the escalation chain table to silently disable a tenant's SOS routing. | Config integrity | Append-only audit log on every change to keywords or escalation chains; daily diff posted to `#infra-nllei01-prod`. | UNAUDITED |
| T3 | Race condition between detector + escalation.Engine -- second SOS for the same IMEI within the dedup window MAY be silently dropped even if it represents a NEW distress event. | SOS event uniqueness | Dedup MUST use IMEI+monotonic-counter, NOT IMEI+payload-hash. Test for monotonic-counter rollover. | UNAUDITED |

### Mitigations already in place

- All inbound webhooks served over TLS 1.3 (k8s ingress).
- HMAC verification with `hmac.Equal()` constant-time comparison.
- WSREP Galera cluster ensures any write that succeeds is replicated to all nodes (no torn writes).

### Open questions

- Is there end-to-end testing that proves a tampered Cloudloop body (e.g. ?text=TEST instead of ?text=SOS) gets rejected at the carrier level before reaching the detector?
- What is the dedup window length, and is it tested against the worst-case satellite latency (Iridium ~30s, Globalstar ~45s)?

## Repudiation

_Adversary denies having performed an action with no audit trail._

### Threats specific to this spec

| # | Threat | Asset | Mitigation | Status |
|---|---|---|---|---|
| R1 | Tenant operator denies that an SOS was received & routed -- no per-event audit row to point to. | Receipt audit | Every SOSEvent MUST persist to `sos_events_audit` table with (carrier, raw_payload_b64, decoded_text, tenant_id, escalation_chain_id, received_at). Retention >= 7 years for life-safety events. | UNAUDITED |
| R2 | Operator who silenced an escalation chain (`acknowledged`) denies the silence action. | Acknowledgement audit | Every state transition (escalated -> acknowledged -> resolved) emits an audit row with user_id, source_ip, user_agent. | UNAUDITED |
| R3 | Tenant claims the escalation chain configuration was different at the time of the incident than what the DB currently shows. | Config-version audit | Escalation chains stored append-only with `version` + `valid_from`/`valid_to`. Audit query reconstructs the chain as it was at `received_at`. | UNAUDITED |

### Mitigations already in place

- `sos_events_audit` table exists (CGC-verified -- schema in migrations/).
- WSREP Galera provides strict consistency; no "lost writes" once committed.

### Open questions

- Is the 7-year retention actually enforced (cron / lifecycle policy), or is it aspirational?
- Does the audit row capture the raw carrier payload BEFORE decode (so a future decoder bug doesn't lose forensic detail)?

## Information Disclosure

_Adversary reads data they should not be able to read._

### Threats specific to this spec

| # | Threat | Asset | Mitigation | Status |
|---|---|---|---|---|
| I1 | Tenant A queries SOS history endpoint and gets Tenant B's SOS events back -- the canonical multi-tenant breach. | Per-tenant SOS history | EVERY read query through `internal/sos/store.go` MUST filter by `tenant_id` derived from JWT claims, NOT from request body / query string. Unit test that calls with manipulated tenant_id in body and asserts 403. | UNAUDITED |
| I2 | Distressed operator's lat/lon leaks via debug log shipped to a non-tenant-isolated log sink. | Position privacy | Position field MUST be redacted in debug logs (replace with `<redacted>`); only the SOS detail page (per-tenant ACL) reveals it. | UNAUDITED |
| I3 | Carrier webhook body contains PII (operator name embedded in MO text) that lands in an error stack trace which gets shipped to Sentry / external APM. | Error reporting | Sentry/APM integration MUST scrub bodies via deny-list at the SDK level (BeforeSend hook). Test: inject "MAYDAY my name is John Doe" -> check Sentry envelope DOES NOT contain "John Doe". | UNAUDITED |

### Mitigations already in place

- Triple-layer tenant isolation (storage + transport + identity) per meshsat-hub constitution Article III.
- `internal/middleware/tenant.go` injects tenant_id from JWT -- never trusts request body.

### Open questions

- Is there a property-based test for tenant isolation across EVERY SOS endpoint (history, ack, resolve, replay)?
- Are we shipping anything to a third party (Sentry, Datadog) and if so, is PII scrubbing actually configured?

## Denial of Service

_Adversary makes the service unavailable to legitimate users._

### Threats specific to this spec

| # | Threat | Asset | Mitigation | Status |
|---|---|---|---|---|
| D1 | Attacker floods the Cloudloop webhook endpoint (allowlist permits it) with bogus SOS bodies, overwhelming escalation.Engine fanout -> real SOS gets backlogged. | Webhook throughput | Per-source-IP rate limit even within allowlist; backpressure on bus produces shed-load on the carrier ingress, NOT on the escalation queue. SLI: `meshsat_hub_sos_escalation_latency_seconds{p99}` MUST stay < 30s. | UNAUDITED |
| D2 | Attacker triggers escalation chains that fan-out to expensive channels (SMS via Twilio, push via ntfy) racking up cost AND saturating outbound bandwidth. | Outbound channel budget | Per-tenant per-day SMS quota; circuit-breaker on Twilio cost > $X/day; alert when burn-rate exceeds budget. | UNAUDITED |
| D3 | Article VII says SOS detector is fail-OPEN -- if the bus is unreachable, real SOS will be silently dropped (correct posture for fail-OPEN, but we need an alarm). | Bus availability | Prometheus alarm: `up{job="bus"} == 0` for >30s pages immediately. Detector logs every failed publish at ERROR level. | UNAUDITED |

### Mitigations already in place

- `meshsat_hub_sos_escalation_latency_seconds` metric exists (CGC-verified in internal/sos/metrics.go).
- Article VII fail-OPEN posture is documented + tested in `chaos_test.go` (pending -- see T-002).

### Open questions

- Is the rate-limit on the Cloudloop webhook calibrated to permit the worst-case BURST of real SOS during a mass-casualty event? (Don't accidentally rate-limit real distress traffic.)
- Does the Twilio circuit-breaker have a TENANT-OVERRIDE for life-safety so a cost cap can't silence a real SOS?

## Elevation of Privilege

_Adversary gains capabilities beyond their authorisation level._

### Threats specific to this spec

| # | Threat | Asset | Mitigation | Status |
|---|---|---|---|---|
| E1 | Tenant operator with `scope:sos.read` discovers an endpoint that grants `sos.ack` via missing scope check. | Scope enforcement | Middleware-level scope assertion on EVERY route (default-deny). Test: a token with only `sos.read` MUST 403 on `/api/v1/sos/{id}/acknowledge`. | UNAUDITED |
| E2 | Attacker registers a fake "escalation channel" of type `webhook` pointing to attacker-controlled URL; subsequent SOS payloads (including position + tenant identifier) are exfiltrated. | Channel registration | Webhook channel URL MUST go through SSRF defender (deny RFC1918, link-local, metadata IPs); require tenant-admin role to add webhook channel; require URL ownership proof (HTTP challenge file). | UNAUDITED |
| E3 | Internal escalation.Engine runs with broader DB privileges than necessary -- a code-injection bug there compromises whole DB. | DB role separation | escalation.Engine connects with a DB user that has SELECT on tenants/escalation_chains, INSERT on sos_events_audit, NO ALTER, NO DROP. Verified via `SHOW GRANTS` smoke test on each deploy. | UNAUDITED |

### Mitigations already in place

- Default-deny middleware in `internal/middleware/auth.go`.
- Per-route scope assertion via `RequireScope("sos.write")` and similar named helpers.

### Open questions

- Has SSRF defender been tested against link-local IPv6 (`fe80::`)?
- Are DB grants actually narrow, or did we ship with `GRANT ALL` and forget?

## Review log

| Date | Reviewer | Action | Notes |
|---|---|---|---|
| 2026-05-18 | sdd-add-stride-threat-model.py | Generated skeleton | All threats marked [NEEDS_REVIEW] |
| 2026-05-18 | Tier 2A hand-author pass | Replaced placeholders with concrete spec-grounded threats | Anchored to CGC-verified internal/sos/detector.go shape + meshsat-hub Articles III, IX, XVII. Threats T1/I1/D1 carry the highest operational priority -- write tests + alerts FIRST. |

## OWASP ASVS / MITRE ATT&CK alignment

| Threat | OWASP ASVS L2 | MITRE ATT&CK |
|---|---|---|
| S1 | V13.2.1 (RESTful Web Services -- Authentication) | T1190 (Exploit Public-Facing Application) |
| S2 | V8.1.6 (Sensitive private data) | T1078.001 (Default Accounts) |
| T1 | V9.1.1 (Communications Security) | T1557 (Adversary-in-the-Middle) |
| T2 | V10.3.1 (Deployed Application Integrity) | T1556 (Modify Authentication Process) |
| R1 | V7.1.1 (Log Content Requirements) | T1070.001 (Indicator Removal: Clear Windows Event Logs) |
| I1 | V8.1.4 (Tenant isolation) | T1078.004 (Cloud Accounts) |
| I2 | V8.3.2 (Sensitive private data redaction) | T1530 (Data from Cloud Storage Object) |
| D1 | V12.2.1 (HTTP Request Header Validation) | T1499 (Endpoint Denial of Service) |
| D2 | V12.1.1 (File Upload Requirements) | T1496 (Resource Hijacking) |
| E1 | V4.1.1 (Access Control) | T1068 (Exploitation for Privilege Escalation) |
| E2 | V13.4.1 (GraphQL & other Web Service issues -- SSRF) | T1090 (Proxy) |
| E3 | V1.10.1 (Software Architecture -- least privilege) | T1098.003 (Account Manipulation: Add Cloud Roles) |

**Next action:** every row with `Status: UNAUDITED` is a gate on this spec moving to status `in-progress`. Security review pass MUST convert each row to `AUDITED-MITIGATED`, `AUDITED-ACCEPTED-RISK` (with `# Expires:` per SLA), or `AUDITED-OUT-OF-SCOPE` (with cross-ref to the spec that DOES cover it).
