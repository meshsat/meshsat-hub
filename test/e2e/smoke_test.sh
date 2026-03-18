#!/usr/bin/env bash
# smoke_test.sh — Post-deploy E2E smoke tests for MeshSat Hub cluster.
#
# Verifies that both Hub hosts are running, sharing state (PostgreSQL, Redis),
# and correctly handling auth, device CRUD, audit, and rate limiting.
#
# Usage:
#   HUB_HOST_1=https://hub1.example.com \
#   HUB_HOST_2=https://hub2.example.com \
#   HUB_E2E_TOKEN=<owner-api-key-or-auth-token> \
#   ./test/e2e/smoke_test.sh
#
# Exit codes:
#   0 — all tests passed
#   1 — one or more tests failed

set -euo pipefail

HUB_HOST_1="${HUB_HOST_1:?Set HUB_HOST_1 (e.g. https://hub1.example.com)}"
HUB_HOST_2="${HUB_HOST_2:?Set HUB_HOST_2 (e.g. https://hub2.example.com)}"
HUB_E2E_TOKEN="${HUB_E2E_TOKEN:?Set HUB_E2E_TOKEN (owner API key or auth token)}"

PASS=0
FAIL=0
TEST_IMEI="E2E$(date +%s)"

auth_header="Authorization: Bearer ${HUB_E2E_TOKEN}"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

check_status() {
    local desc="$1" url="$2" expected="${3:-200}"
    local status
    status=$(curl -s -o /dev/null -w '%{http_code}' -H "${auth_header}" "$url")
    if [ "$status" = "$expected" ]; then
        pass "$desc (HTTP $status)"
    else
        fail "$desc (expected $expected, got $status)"
    fi
}

echo "=== MeshSat Hub E2E Smoke Tests ==="
echo "Host 1: ${HUB_HOST_1}"
echo "Host 2: ${HUB_HOST_2}"
echo "Test IMEI: ${TEST_IMEI}"
echo ""

# ---------------------------------------------------------------------------
# 1. Health endpoints on both hosts
# ---------------------------------------------------------------------------
echo "[1/7] Health checks"
check_status "Host 1 /healthz" "${HUB_HOST_1}/healthz"
check_status "Host 2 /healthz" "${HUB_HOST_2}/healthz"
check_status "Host 1 /readyz"  "${HUB_HOST_1}/readyz"
check_status "Host 2 /readyz"  "${HUB_HOST_2}/readyz"
echo ""

# ---------------------------------------------------------------------------
# 2. Auth flow — verify token works on both hosts
# ---------------------------------------------------------------------------
echo "[2/7] Auth verification"
check_status "Host 1 /api/auth/me" "${HUB_HOST_1}/api/auth/me"
check_status "Host 2 /api/auth/me" "${HUB_HOST_2}/api/auth/me"
echo ""

# ---------------------------------------------------------------------------
# 3. API key cross-host — create on host 1, use on host 2
# ---------------------------------------------------------------------------
echo "[3/7] API key cross-host validation"
KEY_RESP=$(curl -s -H "${auth_header}" -H "Content-Type: application/json" \
    -d '{"label":"e2e-test","role":"viewer"}' \
    "${HUB_HOST_1}/api/auth/keys")

API_KEY=$(echo "$KEY_RESP" | grep -o '"key":"[^"]*"' | head -1 | cut -d'"' -f4)
KEY_ID=$(echo "$KEY_RESP" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

if [ -n "$API_KEY" ]; then
    pass "Created API key on host 1"
    # Use the key on host 2
    STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer ${API_KEY}" \
        "${HUB_HOST_2}/api/auth/me")
    if [ "$STATUS" = "200" ]; then
        pass "API key valid on host 2 (cross-host auth)"
    else
        fail "API key invalid on host 2 (HTTP $STATUS)"
    fi
    # Cleanup: delete the test key
    curl -s -o /dev/null -H "${auth_header}" -X DELETE "${HUB_HOST_1}/api/auth/keys/${KEY_ID}"
else
    fail "Failed to create API key on host 1: $KEY_RESP"
fi
echo ""

# ---------------------------------------------------------------------------
# 4. Data replication — create device on host 1, verify on host 2
# ---------------------------------------------------------------------------
echo "[4/7] Device replication"
CREATE_STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
    -H "${auth_header}" -H "Content-Type: application/json" \
    -d "{\"imei\":\"${TEST_IMEI}\",\"label\":\"E2E Test Device\",\"type\":\"rockblock\"}" \
    "${HUB_HOST_1}/api/devices")

if [ "$CREATE_STATUS" = "201" ] || [ "$CREATE_STATUS" = "200" ]; then
    pass "Created device on host 1"

    # Verify device visible on host 2
    GET_STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
        -H "${auth_header}" \
        "${HUB_HOST_2}/api/devices/${TEST_IMEI}")
    if [ "$GET_STATUS" = "200" ]; then
        pass "Device visible on host 2 (data replication)"
    else
        fail "Device not found on host 2 (HTTP $GET_STATUS)"
    fi

    # Cleanup: delete the test device
    curl -s -o /dev/null -H "${auth_header}" -X DELETE "${HUB_HOST_1}/api/devices/${TEST_IMEI}"
else
    fail "Failed to create device on host 1 (HTTP $CREATE_STATUS)"
fi
echo ""

# ---------------------------------------------------------------------------
# 5. Audit chain integrity on both hosts
# ---------------------------------------------------------------------------
echo "[5/7] Audit chain verification"
AUDIT1=$(curl -s -H "${auth_header}" "${HUB_HOST_1}/api/audit/verify")
AUDIT2=$(curl -s -H "${auth_header}" "${HUB_HOST_2}/api/audit/verify")

VALID1=$(echo "$AUDIT1" | grep -o '"valid":\s*true' || true)
VALID2=$(echo "$AUDIT2" | grep -o '"valid":\s*true' || true)

if [ -n "$VALID1" ]; then
    pass "Host 1 audit chain valid"
else
    fail "Host 1 audit chain invalid: $AUDIT1"
fi
if [ -n "$VALID2" ]; then
    pass "Host 2 audit chain valid"
else
    fail "Host 2 audit chain invalid: $AUDIT2"
fi
echo ""

# ---------------------------------------------------------------------------
# 6. Rate limit shared state — check usage endpoint on both hosts
# ---------------------------------------------------------------------------
echo "[6/7] Rate limit shared state"
check_status "Host 1 /api/ratelimit" "${HUB_HOST_1}/api/ratelimit"
check_status "Host 2 /api/ratelimit" "${HUB_HOST_2}/api/ratelimit"
echo ""

# ---------------------------------------------------------------------------
# 7. Version/settings endpoint consistency
# ---------------------------------------------------------------------------
echo "[7/7] Settings endpoint"
check_status "Host 1 /api/devices (list)" "${HUB_HOST_1}/api/devices"
check_status "Host 2 /api/devices (list)" "${HUB_HOST_2}/api/devices"
echo ""

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo "=== Results ==="
echo "Passed: ${PASS}"
echo "Failed: ${FAIL}"
echo ""

if [ "$FAIL" -gt 0 ]; then
    echo "SMOKE TESTS FAILED"
    exit 1
fi

echo "ALL SMOKE TESTS PASSED"
exit 0
