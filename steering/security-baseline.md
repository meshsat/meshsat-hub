# Steering — Security baseline

Per Article II — security is #1.

## Layer 1 — Article III (strict-decode every input)

`internal/httpjson/readjson.go` is the canonical helper. `httpjson.ReadJSON()` enforces:
- size limit (default 1 MB; per-handler overridable)
- `DisallowUnknownFields()`
- multi-value rejection (`dec.More()`)
- typed-error unwrapping (SyntaxError / UnmarshalTypeError / MaxBytesError / "unknown field" / EOF)

Every HTTP handler in `internal/api/` + sibling packages (cloudloop, wireguard, globalstar, webhook) MUST use it.

## Layer 2 — Article IX (webhook authenticity)

- RockBlock + Globalstar: HMAC-SHA256 verification (provider-supplied secret).
- Cloudloop: IP allowlist scoped to published outbound IPs (`config.DefaultCloudloopWebhookIPs`: 35.178.100.117, 52.56.155.169). Operator override: `HUB_CLOUDLOOP_WEBHOOK_ALLOWED_IPS`.

If Cloudloop ever publishes HMAC, the IP allowlist carve-out collapses.

## Layer 3 — Article IV (triple-layer tenant isolation)

- Storage: SQLite-per-tenant (Tier 1) OR MariaDB schema-partitioning (Tier 2+).
- Transport: MQTT topic ACL `meshsat/{tenant_id}/{device_id}/...`.
- Identity: JWT `tenant_id` claim via `TenantContextKey`.

Every Store interface method has `tenantID string` as second arg.

## Layer 4 — Article VIII (audit log SHA-256 chain)

`internal/audit/` writes a hash-chained JSONL. Tamper detection on chain verify.

## Layer 5 — Article XIV (CI security gates)

- `make lint && make test && gosec ./... && govulncheck ./...` on every push.
- HIGH-severity findings BLOCK merge.
- OWASP ZAP every public URL at each v0.X release.
- A task is NOT done until pipeline passes AND deployment is verified.
