# MeshSat Hub — Security Audit Report

_Version: v0.2 (multi-tenant device management)_
_Date: 2026-03-18_
_Auditor: Claude Code_
_Scope: All Go code + production header audit on NL/GR DMZ hosts_

## Summary

| Category | Finding | Severity | Status |
|----------|---------|----------|--------|
| Security headers | HSTS, CSP, X-Frame-Options, nosniff, Referrer-Policy, Permissions-Policy | High | **Fixed** (MESHSAT-161) |
| MQTT reconnect subscription loss | Silent message loss after broker restart | Critical | **Fixed** (MESHSAT-154) |
| Go stdlib vulnerabilities | 18 CVEs in go1.24.1 | Medium | Mitigated by Caddy/nginx in prod |
| Path traversal in backup import | ZIP entry names not sanitized | High | **Fixed** (v0.1) |
| Request body size on webhook | No limit on rockblock POST body | Medium | **Fixed** (v0.1, 1MB limit) |
| SQL injection | All queries parameterized ($1/$2 for PG, ? for SQLite) | N/A | Clean |
| Hardcoded secrets | None found | N/A | Clean |
| Auth middleware | OAuth2/OIDC + API keys + RBAC (viewer/operator/owner) | N/A | Implemented (v0.2) |
| Tenant isolation | Per-tenant DB queries, MQTT topic prefix, JWT claim | N/A | Implemented (v0.2) |
| Audit log | Tamper-evident SHA-256 hash chain | N/A | Implemented (v0.2) |
| MQTT ACLs | Anonymous access on internal broker | Low | Acceptable for Docker network |
| Unbounded goroutines | API key middleware spawns per-request goroutine | Low | Tracked (MESHSAT-158) |

## 1. Security Headers (MESHSAT-161) — Fixed

**Before (v0.2 pre-fix):** No security headers on any response. Both production hosts returned only nginx default headers.

**After (MESHSAT-161):** All HTTP responses include:
- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY`
- `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https://*.tile.openstreetmap.org; connect-src 'self'; font-src 'self'; object-src 'none'; frame-ancestors 'none'`
- `Referrer-Policy: strict-origin-when-cross-origin`
- `Permissions-Policy: camera=(), microphone=(), geolocation=()`
- `Strict-Transport-Security: max-age=31536000; includeSubDomains` (only when behind TLS)

**CSP notes:** `style-src 'unsafe-inline'` required for Tailwind CSS runtime styles. `img-src` includes OpenStreetMap tile CDN for Leaflet map.

## 2. MQTT Reconnect Bug (MESHSAT-154) — Fixed

**Before:** Paho MQTT client reconnected to broker after restart, but all subscriptions were silently lost. mtSender, webhookDispatcher, TAK subscriber, and APRS-IS subscriber stopped receiving messages. Service appeared healthy (/healthz 200, /readyz mqtt:ok) but processed nothing.

**After:** Subscription registry in Paho Bus records all Subscribe/QueueSubscribe calls and replays them in OnConnectHandler after reconnect. Verified with unit tests.

## 3. govulncheck Results

18 vulnerabilities found in Go standard library (go1.24.1):

- `crypto/tls`: handshake level, session resumption, ALPN (GO-2026-4340, 4337, 4008)
- `crypto/x509`: name constraints, DSA panic, wildcard names (GO-2025-4175, 4155, 4013, 4007, 3749)
- `net/url`: IPv6 parsing, query parameter DoS (GO-2026-4601, 4341)
- `net/http`: cookie parsing DoS, request smuggling, sensitive headers (GO-2025-4012, 3563, 3751)
- `encoding/pem`: quadratic complexity (GO-2025-4009)
- `os`: FileInfo escape from Root (GO-2026-4602)

**Mitigation:** Update Go toolchain to 1.24.13+ when available. In production, nginx reverse proxy handles TLS termination and HTTP parsing.

**No vulnerabilities in third-party dependencies.**

## 4. Authentication & Authorization (v0.2)

| Layer | Mechanism | Status |
|-------|-----------|--------|
| Auth modes | none / token / OIDC (auto-detect) | Implemented |
| API keys | `meshsat_` prefixed, SHA-256 hashed at rest, expiry, RBAC roles | Implemented |
| RBAC | viewer < operator < owner hierarchy, RequireRole middleware | Implemented |
| Tenant isolation | JWT `tenant_id` claim, X-Tenant-ID header fallback, per-query filtering | Implemented |
| Rate limiting | Per-device token bucket + daily/monthly caps, SOS bypass | Implemented |
| Audit | Tamper-evident SHA-256 hash chain, tenant-isolated, verify endpoint | Implemented |

**Exempt paths (no auth required):**
- `/healthz`, `/readyz` — health probes
- `/api/webhook/rockblock` — uses its own HMAC-SHA256 signature verification

## 5. Input Validation

| Endpoint | Validation |
|----------|-----------|
| `/api/webhook/rockblock` | HMAC-SHA256 signature, form field presence, hex decode, 1MB body limit |
| `/api/devices` | JSON decode, IMEI format |
| `/api/devices/{imei}/config` | JSON decode, version validation |
| `/api/auth/keys` | JSON decode, role validation, owner-only |
| `/api/ratelimit/{id}/override` | JSON decode, positive duration |
| `/api/webhooks` | JSON decode, URL required |
| `/api/backup/import` | ZIP format, manifest.json presence, path traversal guard, 50MB limit |

## 6. SQL Injection Prevention

All database queries use parameterized statements:
- **SQLite:** `?` placeholders via `modernc.org/sqlite` (pure Go, no CGO)
- **PostgreSQL:** `$1`, `$2` placeholders via `pgx`
- No string concatenation in any SQL query (verified by grep audit)

## 7. Secrets Management

- All secrets via environment variables or YAML config file
- No secrets in code, logs, or error messages
- Backup export redacts secrets from config manifest
- Webhook listing redacts HMAC secrets
- API keys stored as SHA-256 hashes (raw key shown only on creation)
- Audit log entries never contain credential material

## 8. Production Header Audit (2026-03-18)

**Hosts tested:** `nllei01dmz01:8451` (NL), `grskg01dmz01:8451` (GR)

Pre-MESHSAT-161 deployment (baseline):
```
Server: nginx/1.29.6
Content-Type: text/html; charset=utf-8  (or application/json)
No security headers present
```

Both hosts sit behind nginx reverse proxy with TLS termination at port 8451.
Auth middleware verified: `/api/auth/me` returns 401 Unauthorized without token.

**Post-MESHSAT-161 deployment:** Pipeline running (ID 12587). Security headers will be verified after deployment completes.

## 9. Known Issues (Tracked)

| Issue | Severity | YouTrack | Status |
|-------|----------|----------|--------|
| Unbounded goroutines in API key middleware | Low | MESHSAT-158 | Open |
| k8s.io/client-go pulls 40+ transitive deps | Low | MESHSAT-155 | Open |
| API handler unit tests at 0% coverage | Medium | MESHSAT-156 | Open |
| 18/28 Swagger annotations missing | Low | MESHSAT-157 | Open |

## 10. Recommendations

1. **Upgrade Go** to 1.24.13+ when CI builder image is updated
2. **Enable MQTT authentication** in production deployments
3. **Add goroutine pool** for API key `TouchLastUsed` writes (MESHSAT-158)
4. **Run OWASP ZAP** full active scan when public DNS is configured
5. **Add API handler unit tests** for error paths and edge cases (MESHSAT-156)
6. **Monitor CSP reports** — consider adding `report-uri` directive when logging infra supports it
