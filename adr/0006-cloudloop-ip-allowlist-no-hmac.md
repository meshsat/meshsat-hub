# 6. Cloudloop IP allowlist (no HMAC available)

Date: 2026-05-18

## Status
Accepted

## Context
Cloudloop is one of the satellite-provider webhook sources. Unlike RockBLOCK and Globalstar (HMAC-SHA256 signed payloads), Cloudloop's documented webhook authentication (per knowledge.cloudloop.com/docs/destinations/http) is IP allowlist only — no HMAC offered.

## Decision
Article IX carve-out: Cloudloop webhooks authenticated via strict IP allowlist scoped to Cloudloop's published outbound IPs.

Real implementation (CGC-verified):
- `internal/config/config.go` `DefaultCloudloopWebhookIPs = "35.178.100.117,52.56.155.169"` (Cloudloop's documented outbound IPs)
- `internal/config/config.go` `ResolveCloudloopAllowedIPs(operatorOverride string) (ips []string, fromOperator bool)`
- Operator override via `HUB_CLOUDLOOP_WEBHOOK_ALLOWED_IPS` env var (comma-separated)
- Webhook handler at `internal/cloudloop/webhook.go` rejects on IP mismatch

## Consequences
**Positive:** Best authentication Cloudloop currently allows. Operator can extend allowlist for testing.
**Negative:** IP allowlist is weaker than HMAC — Cloudloop's outbound IPs could be DNS-rebound or BGP-hijacked. Mitigation: monitor for IP changes; collapse to HMAC immediately if Cloudloop publishes it.

**Enforced by:** Article IX carve-out + `ResolveCloudloopAllowedIPs` test in `internal/config/config_test.go`.
