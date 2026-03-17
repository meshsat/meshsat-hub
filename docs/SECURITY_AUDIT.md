# MeshSat Hub — Security Audit Report

_Date: 2026-03-17_
_Auditor: Claude Code_
_Scope: All Go code in meshsat-hub_

## Summary

| Category | Finding | Severity | Status |
|----------|---------|----------|--------|
| Go stdlib vulnerabilities | 18 CVEs in go1.24.1 | Medium | Mitigated by Caddy/HAProxy in prod |
| Path traversal in backup import | ZIP entry names not sanitized | High | **Fixed** |
| Request body size on webhook | No limit on rockblock POST body | Medium | **Fixed** (1MB limit) |
| SQL injection | No SQL in codebase (stateless) | N/A | Clean |
| Hardcoded secrets | None found | N/A | Clean |
| XSS/CSRF | No HTML rendering, API-only | N/A | Not applicable |
| MQTT ACLs | Anonymous access on internal broker | Low | Acceptable for Docker network |
| Auth on API endpoints | Single bearer token (`HUB_AUTH_TOKEN`) | Low | Acceptable for v0.1 |

## 1. govulncheck Results

18 vulnerabilities found, all in Go standard library (go1.24.1 → fixed in go1.24.12+):

- `crypto/tls`: handshake level, session resumption, ALPN (GO-2026-4340, 4337, 4008)
- `crypto/x509`: name constraints, DSA panic, wildcard names (GO-2025-4175, 4155, 4013, 4007, 3749)
- `net/url`: IPv6 parsing, query parameter DoS (GO-2026-4601, 4341)
- `net/http`: cookie parsing DoS, request smuggling, sensitive headers (GO-2025-4012, 3563, 3751)
- `encoding/pem`: quadratic complexity (GO-2025-4009)
- `os`: FileInfo escape from Root (GO-2026-4602)

**Mitigation:** Update Go toolchain to 1.24.13+ when available. In production, Caddy reverse proxy handles TLS termination and HTTP parsing, shielding the Hub from most of these.

**No vulnerabilities in third-party dependencies.**

## 2. Path Traversal (Fixed)

`internal/backup/backup.go` Import() used ZIP entry names directly in `filepath.Join()` without sanitization. A malicious ZIP with `data/../../../etc/passwd` could write outside the data directory.

**Fix:** Added `strings.HasPrefix(filepath.Clean(destPath), filepath.Clean(dataDir))` guard. Blocked paths are logged and skipped.

## 3. Request Body Limits (Fixed)

`internal/rockblock/handler.go` had no body size limit on the webhook POST handler. An attacker could send a multi-GB request to exhaust memory.

**Fix:** Added `http.MaxBytesReader(w, r.Body, 1<<20)` (1MB limit). RockBLOCK payloads are max 340 bytes, so 1MB is generous.

Backup endpoints already had `io.LimitReader` at 50MB.

## 4. No SQL Injection Surface

Hub is stateless — no database, no SQL queries. All data flows through MQTT pub/sub. No injection risk.

## 5. Secrets Management

- All secrets via environment variables (`HUB_ROCKBLOCK_SECRET`, `HUB_CLOUDLOOP_API_KEY`, `HUB_AUTH_TOKEN`, `HUB_APRSIS_PASSCODE`)
- No secrets in code, config files, or logs
- Backup export redacts secrets from config
- Webhook listing redacts HMAC secrets

## 6. Input Validation

| Endpoint | Validation |
|----------|-----------|
| `/api/webhook/rockblock` | HMAC-SHA256 or JWT signature, form field presence, hex decode |
| `/api/ratelimit/{id}/override` | JSON decode, positive duration |
| `/api/webhooks` | JSON decode, URL required |
| `/api/backup/import` | ZIP format, manifest.json presence, path traversal guard |

## 7. MQTT Security

Internal Mosquitto broker allows anonymous access within the Docker network. This is acceptable because:
- Port 6071 is only exposed for field device connections (should be firewalled)
- In production, MQTT auth should be enabled (documented in deployment.md)
- Future: MQTT ACLs per device (v0.2 multi-tenancy)

## 8. Recommendations

1. **Upgrade Go** to 1.24.13+ when CI builder image is updated
2. **Enable MQTT authentication** in production deployments (documented)
3. **Add rate limiting** on `/api/webhook/rockblock` (Caddy handles this in prod)
4. **Add OAuth2/OIDC** auth middleware when dashboard is built (v0.2)
5. **Add CORS headers** if dashboard serves from a different origin
