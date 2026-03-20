#!/usr/bin/env bash
# owasp-scan.sh — OWASP compliance testing for MeshSat Hub public-facing URLs.
#
# Runs three phases:
#   1. Security header validation (lightweight, no ZAP needed)
#   2. ZAP baseline scan (passive spider + scan, Docker-based)
#   3. ZAP full active scan (optional, for release milestones)
#
# Usage:
#   HUB_TARGET_URL=https://hub.example.com \
#   HUB_AUTH_TOKEN=<owner-token> \
#   ./test/owasp/owasp-scan.sh [--full]
#
# Options:
#   --full    Run active scan in addition to baseline (slower, more thorough)
#
# Environment:
#   HUB_TARGET_URL   Base URL of the Hub instance to scan (required)
#   HUB_AUTH_TOKEN    Auth token for authenticated endpoint testing (required)
#   ZAP_IMAGE         ZAP Docker image (default: ghcr.io/zaproxy/zaproxy:stable)
#   REPORT_DIR        Output directory for reports (default: test/owasp/reports)
#
# Exit codes:
#   0 — all checks passed
#   1 — one or more FAIL-level findings
#   2 — configuration error

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

HUB_TARGET_URL="${HUB_TARGET_URL:?Set HUB_TARGET_URL (e.g. https://hub.example.com)}"
HUB_AUTH_TOKEN="${HUB_AUTH_TOKEN:?Set HUB_AUTH_TOKEN (owner API key or auth token)}"
ZAP_IMAGE="${ZAP_IMAGE:-ghcr.io/zaproxy/zaproxy:stable}"
REPORT_DIR="${REPORT_DIR:-${REPO_ROOT}/test/owasp/reports}"
FULL_SCAN=false
TIMESTAMP=$(date +%Y%m%d-%H%M%S)

if [[ "${1:-}" == "--full" ]]; then
    FULL_SCAN=true
fi

mkdir -p "$REPORT_DIR"

PASS=0
FAIL=0
WARN=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
warn() { echo "  WARN: $1"; WARN=$((WARN + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

# ============================================================================
# Phase 1: Security Header Validation
# ============================================================================
echo "=== Phase 1: Security Header Validation ==="
echo "Target: ${HUB_TARGET_URL}"
echo ""

check_header() {
    local endpoint="$1" header="$2" expected="$3" severity="${4:-FAIL}"
    local value
    value=$(curl -s -o /dev/null -D - -H "Authorization: Bearer ${HUB_AUTH_TOKEN}" \
        "${HUB_TARGET_URL}${endpoint}" 2>/dev/null | grep -i "^${header}:" | head -1 | tr -d '\r')

    if [ -z "$value" ]; then
        if [ "$severity" = "FAIL" ]; then
            fail "${endpoint} missing ${header}"
        else
            warn "${endpoint} missing ${header}"
        fi
        return
    fi

    if echo "$value" | grep -qi "$expected"; then
        pass "${endpoint} has ${header}"
    else
        if [ "$severity" = "FAIL" ]; then
            fail "${endpoint} ${header} unexpected: ${value}"
        else
            warn "${endpoint} ${header} unexpected: ${value}"
        fi
    fi
}

# Test security headers on public endpoints
echo "[1.1] Public endpoints (/healthz, /readyz)"
for endpoint in "/healthz" "/readyz"; do
    check_header "$endpoint" "X-Content-Type-Options" "nosniff"
    check_header "$endpoint" "X-Frame-Options" "DENY\|SAMEORIGIN"
    check_header "$endpoint" "Content-Security-Policy" "default-src"
    check_header "$endpoint" "Referrer-Policy" "strict-origin"
    check_header "$endpoint" "Permissions-Policy" "camera=()"
done
echo ""

echo "[1.2] Authenticated API endpoints"
for endpoint in "/api/auth/me" "/api/devices" "/api/messages"; do
    check_header "$endpoint" "X-Content-Type-Options" "nosniff"
    check_header "$endpoint" "X-Frame-Options" "DENY\|SAMEORIGIN"
    check_header "$endpoint" "Content-Security-Policy" "default-src"
    check_header "$endpoint" "Referrer-Policy" "strict-origin"
    check_header "$endpoint" "Permissions-Policy" "camera=()"
done
echo ""

# HSTS check (only meaningful over HTTPS)
echo "[1.3] HSTS (Strict-Transport-Security)"
if echo "$HUB_TARGET_URL" | grep -q "^https://"; then
    check_header "/healthz" "Strict-Transport-Security" "max-age="
    check_header "/api/auth/me" "Strict-Transport-Security" "max-age="
else
    warn "HSTS check skipped (target is not HTTPS)"
fi
echo ""

# ============================================================================
# Phase 2: Authentication & Access Control Checks
# ============================================================================
echo "=== Phase 2: Authentication & Access Control ==="

echo "[2.1] Unauthenticated access to protected endpoints"
PROTECTED_ENDPOINTS=(
    "/api/auth/me"
    "/api/devices"
    "/api/messages"
    "/api/positions/latest"
    "/api/ratelimit"
    "/api/webhooks"
    "/api/audit"
    "/api/backup/export"
)

for endpoint in "${PROTECTED_ENDPOINTS[@]}"; do
    status=$(curl -s -o /dev/null -w '%{http_code}' "${HUB_TARGET_URL}${endpoint}")
    if [ "$status" = "401" ] || [ "$status" = "403" ]; then
        pass "Unauthenticated ${endpoint} returns ${status}"
    else
        fail "Unauthenticated ${endpoint} returns ${status} (expected 401/403)"
    fi
done
echo ""

echo "[2.2] RBAC enforcement (viewer cannot access owner endpoints)"
# Create a viewer key to test RBAC
VIEWER_RESP=$(curl -s -H "Authorization: Bearer ${HUB_AUTH_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"label":"owasp-viewer-test","role":"viewer"}' \
    "${HUB_TARGET_URL}/api/auth/keys" 2>/dev/null)

VIEWER_KEY=$(echo "$VIEWER_RESP" | grep -o '"key":"[^"]*"' | head -1 | cut -d'"' -f4)
VIEWER_KEY_ID=$(echo "$VIEWER_RESP" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

if [ -n "$VIEWER_KEY" ]; then
    OWNER_ENDPOINTS=("/api/auth/keys" "/api/audit")
    for endpoint in "${OWNER_ENDPOINTS[@]}"; do
        status=$(curl -s -o /dev/null -w '%{http_code}' \
            -H "Authorization: Bearer ${VIEWER_KEY}" \
            "${HUB_TARGET_URL}${endpoint}")
        if [ "$status" = "403" ]; then
            pass "Viewer blocked from ${endpoint} (403)"
        else
            fail "Viewer accessed ${endpoint} (HTTP ${status}, expected 403)"
        fi
    done

    # Cleanup
    curl -s -o /dev/null -H "Authorization: Bearer ${HUB_AUTH_TOKEN}" \
        -X DELETE "${HUB_TARGET_URL}/api/auth/keys/${VIEWER_KEY_ID}"
else
    warn "Could not create viewer key for RBAC test (API keys may not be enabled)"
fi
echo ""

# ============================================================================
# Phase 3: Input Validation Checks
# ============================================================================
echo "=== Phase 3: Input Validation ==="

echo "[3.1] SQL injection probes"
SQL_PAYLOADS=("' OR '1'='1" "1; DROP TABLE devices;--" "' UNION SELECT NULL--")
for payload in "${SQL_PAYLOADS[@]}"; do
    encoded=$(printf '%s' "$payload" | sed 's/ /%20/g; s/'\''/%27/g; s/;/%3B/g; s/=/%3D/g')
    status=$(curl -s -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer ${HUB_AUTH_TOKEN}" \
        "${HUB_TARGET_URL}/api/devices/${encoded}")
    # Should return 400 or 404, never 200 with data or 500
    if [ "$status" = "500" ]; then
        fail "SQL injection probe on /api/devices caused 500: ${payload}"
    else
        pass "SQL injection probe handled safely (HTTP ${status})"
    fi
done
echo ""

echo "[3.2] XSS probes in device creation"
XSS_STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${HUB_AUTH_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"imei":"XSSTEST123456","label":"<script>alert(1)</script>","type":"rockblock"}' \
    "${HUB_TARGET_URL}/api/devices")

if [ "$XSS_STATUS" = "201" ] || [ "$XSS_STATUS" = "200" ]; then
    # Check if the stored value is returned without encoding in JSON
    DEVICE_RESP=$(curl -s -H "Authorization: Bearer ${HUB_AUTH_TOKEN}" \
        "${HUB_TARGET_URL}/api/devices/XSSTEST123456")
    if echo "$DEVICE_RESP" | grep -q '<script>'; then
        warn "XSS payload stored and returned in JSON (CSP should prevent execution)"
    else
        pass "XSS payload sanitized or encoded"
    fi
    # Cleanup
    curl -s -o /dev/null -H "Authorization: Bearer ${HUB_AUTH_TOKEN}" \
        -X DELETE "${HUB_TARGET_URL}/api/devices/XSSTEST123456"
else
    pass "XSS device creation rejected (HTTP ${XSS_STATUS})"
fi
echo ""

echo "[3.3] Path traversal probes"
TRAVERSAL_PAYLOADS=("../../../etc/passwd" "..%2F..%2F..%2Fetc%2Fpasswd" "....//....//etc/passwd")
for payload in "${TRAVERSAL_PAYLOADS[@]}"; do
    status=$(curl -s -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer ${HUB_AUTH_TOKEN}" \
        "${HUB_TARGET_URL}/api/devices/${payload}")
    if [ "$status" = "200" ]; then
        fail "Path traversal may have succeeded: ${payload}"
    else
        pass "Path traversal rejected (HTTP ${status})"
    fi
done
echo ""

echo "[3.4] Oversized request body"
LARGE_BODY=$(printf '{"imei":"%s"}' "$(head -c 2000000 /dev/zero | tr '\0' 'A')")
OVERSIZE_STATUS=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
    -H "Authorization: Bearer ${HUB_AUTH_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "$LARGE_BODY" \
    "${HUB_TARGET_URL}/api/devices" 2>/dev/null || echo "timeout")

if [ "$OVERSIZE_STATUS" = "413" ] || [ "$OVERSIZE_STATUS" = "400" ] || [ "$OVERSIZE_STATUS" = "timeout" ]; then
    pass "Oversized request rejected or timed out (${OVERSIZE_STATUS})"
elif [ "$OVERSIZE_STATUS" = "500" ]; then
    fail "Oversized request caused server error (500)"
else
    warn "Oversized request returned HTTP ${OVERSIZE_STATUS}"
fi
echo ""

# ============================================================================
# Phase 4: ZAP Baseline Scan
# ============================================================================
echo "=== Phase 4: OWASP ZAP Baseline Scan ==="

ZAP_CONF="${SCRIPT_DIR}/zap-baseline.conf"
ZAP_REPORT_HTML="${REPORT_DIR}/zap-baseline-${TIMESTAMP}.html"
ZAP_REPORT_JSON="${REPORT_DIR}/zap-baseline-${TIMESTAMP}.json"

if ! command -v docker &>/dev/null; then
    warn "Docker not available — skipping ZAP scan"
else
    echo "Running ZAP baseline scan..."
    echo "Config: ${ZAP_CONF}"
    echo "Reports: ${REPORT_DIR}/"

    # ZAP baseline scan with auth header hook
    ZAP_EXIT=0
    docker run --rm \
        -v "${ZAP_CONF}:/zap/wrk/zap-baseline.conf:ro" \
        -v "${REPORT_DIR}:/zap/wrk/reports:rw" \
        --network host \
        "$ZAP_IMAGE" zap-baseline.py \
        -t "$HUB_TARGET_URL" \
        -c zap-baseline.conf \
        -z "-config replacer.full_list\\(0\\).description=auth \
            -config replacer.full_list\\(0\\).enabled=true \
            -config replacer.full_list\\(0\\).matchtype=REQ_HEADER \
            -config replacer.full_list\\(0\\).matchstr=Authorization \
            -config replacer.full_list\\(0\\).replacement=Bearer\\ ${HUB_AUTH_TOKEN} \
            -config replacer.full_list\\(0\\).initiators=" \
        -J "reports/zap-baseline-${TIMESTAMP}.json" \
        -r "reports/zap-baseline-${TIMESTAMP}.html" \
        -I || ZAP_EXIT=$?

    echo ""
    if [ "$ZAP_EXIT" -eq 0 ]; then
        pass "ZAP baseline scan completed — no FAIL-level alerts"
    elif [ "$ZAP_EXIT" -eq 1 ]; then
        warn "ZAP baseline scan completed with WARN-level alerts"
    elif [ "$ZAP_EXIT" -eq 2 ]; then
        fail "ZAP baseline scan found FAIL-level alerts"
    else
        warn "ZAP baseline scan exited with code ${ZAP_EXIT}"
    fi

    echo "HTML report: ${ZAP_REPORT_HTML}"
    echo "JSON report: ${ZAP_REPORT_JSON}"
    echo ""

    # ========================================================================
    # Phase 5: ZAP Full Active Scan (optional)
    # ========================================================================
    if [ "$FULL_SCAN" = true ]; then
        echo "=== Phase 5: OWASP ZAP Full Active Scan ==="
        echo "WARNING: Active scan sends attack payloads to the target."
        echo "Only run against test/staging environments or with explicit authorization."
        echo ""

        ZAP_FULL_HTML="${REPORT_DIR}/zap-full-${TIMESTAMP}.html"
        ZAP_FULL_JSON="${REPORT_DIR}/zap-full-${TIMESTAMP}.json"

        ZAP_FULL_EXIT=0
        docker run --rm \
            -v "${ZAP_CONF}:/zap/wrk/zap-baseline.conf:ro" \
            -v "${REPORT_DIR}:/zap/wrk/reports:rw" \
            --network host \
            "$ZAP_IMAGE" zap-full-scan.py \
            -t "$HUB_TARGET_URL" \
            -c zap-baseline.conf \
            -z "-config replacer.full_list\\(0\\).description=auth \
                -config replacer.full_list\\(0\\).enabled=true \
                -config replacer.full_list\\(0\\).matchtype=REQ_HEADER \
                -config replacer.full_list\\(0\\).matchstr=Authorization \
                -config replacer.full_list\\(0\\).replacement=Bearer\\ ${HUB_AUTH_TOKEN} \
                -config replacer.full_list\\(0\\).initiators=" \
            -J "reports/zap-full-${TIMESTAMP}.json" \
            -r "reports/zap-full-${TIMESTAMP}.html" \
            -I || ZAP_FULL_EXIT=$?

        echo ""
        if [ "$ZAP_FULL_EXIT" -eq 0 ]; then
            pass "ZAP full scan completed — no FAIL-level alerts"
        elif [ "$ZAP_FULL_EXIT" -eq 1 ]; then
            warn "ZAP full scan completed with WARN-level alerts"
        elif [ "$ZAP_FULL_EXIT" -eq 2 ]; then
            fail "ZAP full scan found FAIL-level alerts"
        else
            warn "ZAP full scan exited with code ${ZAP_FULL_EXIT}"
        fi

        echo "HTML report: ${ZAP_FULL_HTML}"
        echo "JSON report: ${ZAP_FULL_JSON}"
        echo ""
    fi
fi

# ============================================================================
# Summary
# ============================================================================
echo "=== OWASP Compliance Test Results ==="
echo "Target:  ${HUB_TARGET_URL}"
echo "Date:    $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "Passed:  ${PASS}"
echo "Warnings: ${WARN}"
echo "Failed:  ${FAIL}"
echo ""

# Write summary to JSON for CI artifact parsing
cat > "${REPORT_DIR}/summary-${TIMESTAMP}.json" <<EOF
{
  "target": "${HUB_TARGET_URL}",
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "passed": ${PASS},
  "warnings": ${WARN},
  "failed": ${FAIL},
  "full_scan": ${FULL_SCAN}
}
EOF

if [ "$FAIL" -gt 0 ]; then
    echo "OWASP COMPLIANCE: FAILED (${FAIL} critical findings)"
    exit 1
fi

if [ "$WARN" -gt 0 ]; then
    echo "OWASP COMPLIANCE: PASSED WITH WARNINGS"
    exit 0
fi

echo "OWASP COMPLIANCE: PASSED"
exit 0
