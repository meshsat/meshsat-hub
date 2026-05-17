# Steering — Security baseline (MeshSat Hub)

**Security is the #1 priority** (Constitution Article II). This document is enforced via CI + manual review.

## Secrets

- NEVER commit secrets. `.env*` gitignored. All credentials via env vars (production: systemd-managed) or operator-provisioned config files.
- mTLS keys for Hub↔Bridge: stored in DB-per-tenant, never on filesystem path.
- JWT signing keys: rotated per `internal/auth/rotation.go`. Old keys retained for JWKS validation during cutover window.
- API keys: hashed via Argon2id before storage. NEVER stored plaintext.
- Backups MUST redact secret columns. Verify via `make backup-test`.
- If a secret leaks via commit: immediate rotation + force-cleanup (operator-only escalation).

## Authentication

- Modes: `none` (dev only), `token` (legacy), `oidc` (production primary), `local` (Argon2id passwords).
- All HTTP handlers require auth — except `/healthz`, `/readyz`, `/startupz`, and webhook endpoints that verify their own HMAC signature.
- JWT access tokens: 15min TTL. Refresh tokens: 7-day TTL, HttpOnly SameSite=Strict cookie.
- RBAC: 3 roles (`viewer` < `operator` < `owner`) per `internal/auth/rbac.go:9-13`.
- API keys + JWTs propagate `tenant_id` claim — every handler MUST honor it (Article IV).

## Input validation (Article III)

- Use `internal/api/validate.go` `readJSON()` — NEVER `json.NewDecoder(r.Body).Decode()`.
- All SQL parameterised (`?`). Static analyser flags string-concat into queries.
- File uploads size-limited at handler edge + content-type validated.
- Webhook HMAC verification BEFORE business-logic processing.

## Multi-tenant isolation (Article IV)

- Storage: tenant-file-per-tenant SQLite (Tier 1) OR schema-isolated (Tier 2+).
- MQTT: topic ACL prefix `meshsat/{tenant_id}/{device_id}/...`.
- JWT: `tenant_id` claim, propagated via `TenantContextKey` context value.
- Every Store method takes `tenantID` — code review rejects any handler that loses it.

## Cryptography

- AES-256-GCM `[12B nonce][ciphertext + 16B auth tag]` — IDENTICAL across bridge + hub + android (CLAUDE.md L475-476).
- SMAZ2 dictionary byte-for-byte mirror (Article VI).
- TLS 1.3 only on public endpoints. Ciphers per NIST SP 800-52.
- mTLS for Hub↔Bridge (X.509 ECDSA P-256 client certs, 90-day leaf, 5-year root).
- HAProxy in SNI-passthrough mode — preserves end-to-end mTLS chain.

## Audit log (Article VIII)

- `internal/audit/audit.go` — SHA-256 hash chain, append-only.
- Logs every: login, role change, key rotation, bridge add/delete, MT send, SOS trigger, deadman-switch fire.
- Retention goroutine archives to JSONL BEFORE purging. Archive path MUST exist.
- Hash chain verification on startup; corrupted chain blocks Hub start (operator escalation).

## Network posture

- Hub is internet-exposed. Treat all inbound as hostile until validated.
- HAProxy fronts both DC (NL + GR active-active).
- HSTS, CSP, CORS, X-Frame-Options on all HTTP responses (MESHSAT-161).
- Rate limiting via `internal/ratelimit/` (memory mode standalone, Redis mode cluster). SOS bypass explicit.

## Dependency posture (Article XV)

- `make security` = `gosec ./... && govulncheck ./...`. HIGH severity blocks merge.
- Every new third-party dep: ADR with license + CVE count + maintenance signal + transitive dep count.
- Pin versions in go.mod / package.json. Periodic `go mod tidy` + `npm update` reviewed manually.

## Continuous validation (Article XVII)

- Every push: lint + test + gosec + govulncheck.
- Every v0.X release: OWASP ZAP baseline + active scan (`make owasp` / `make owasp-full`).
- Galera health gate: `scripts/check-galera-health.sh` blocks deploy if cluster size != 3 or wsrep_ready != ON.

## Incident response

- Postmortems in `docs/INCIDENT_NN_POSTMORTEM.md` (currently 11/13/14).
- Runbook in `docs/RUNBOOK.md`.
- Galera-specific recovery: `scripts/galera-entrypoint.sh` (15 safety layers) + `deploy/ansible/playbooks/recover.yml`.
