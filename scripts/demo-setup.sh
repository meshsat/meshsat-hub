#!/usr/bin/env bash
# demo-setup.sh — One-command setup for the MeshSat end-to-end demo.
# Prepares Hub with device, encryption key, routing rule, PGP key exchange.
#
# Usage:
#   ./scripts/demo-setup.sh \
#     --hub-url https://meshsat.example.com \
#     --token YOUR_API_TOKEN \
#     --imei 300234063904190 \
#     --email operator@example.com \
#     --pgp-key /path/to/operator-pubkey.asc
#
# What it does:
#   1. Registers device (idempotent)
#   2. Creates E2E encryption key (prints key_hex for Android)
#   3. Creates routing rule: iridium → email (idempotent)
#   4. Creates SOS escalation chain with email target (idempotent)
#   5. Imports recipient PGP public key
#   6. Exports Hub PGP public key (for Thunderbird import)
#
# Idempotent — safe to run multiple times.
#
set -euo pipefail

# --- Argument parsing ---
HUB_URL=""
TOKEN=""
IMEI=""
EMAIL=""
PGP_KEY_FILE=""

while [[ $# -gt 0 ]]; do
  case $1 in
    --hub-url)   HUB_URL="$2"; shift 2 ;;
    --token)     TOKEN="$2"; shift 2 ;;
    --imei)      IMEI="$2"; shift 2 ;;
    --email)     EMAIL="$2"; shift 2 ;;
    --pgp-key)   PGP_KEY_FILE="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: $0 --hub-url URL --token TOKEN --imei IMEI --email EMAIL [--pgp-key FILE]"
      exit 0 ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

if [[ -z "$HUB_URL" || -z "$TOKEN" || -z "$IMEI" || -z "$EMAIL" ]]; then
  echo "ERROR: --hub-url, --token, --imei, and --email are required"
  echo "Usage: $0 --hub-url URL --token TOKEN --imei IMEI --email EMAIL [--pgp-key FILE]"
  exit 1
fi

# Strip trailing slash from HUB_URL
HUB_URL="${HUB_URL%/}"

AUTH="Authorization: Bearer $TOKEN"
CT="Content-Type: application/json"

ok() { echo "  ✓ $1"; }
fail() { echo "  ✗ $1"; }
step() { echo ""; echo "[$1] $2"; }

# --- 1. Register device ---
step 1 "Registering device $IMEI"
# Check if device already exists
RESP=$(curl -s -w "\n%{http_code}" "$HUB_URL/api/devices/$IMEI" -H "$AUTH")
HTTP_CODE=$(echo "$RESP" | tail -1)
if [[ "$HTTP_CODE" == "200" ]]; then
  ok "Device already exists (idempotent)"
else
  RESP=$(curl -s -w "\n%{http_code}" -X POST "$HUB_URL/api/devices" \
    -H "$AUTH" -H "$CT" \
    -d "{\"imei\":\"$IMEI\",\"label\":\"Demo Device\",\"type\":\"rockblock\"}")
  HTTP_CODE=$(echo "$RESP" | tail -1)
  BODY=$(echo "$RESP" | sed '$d')
  if [[ "$HTTP_CODE" == "201" ]]; then
    ok "Device registered"
  elif [[ "$HTTP_CODE" == "409" ]]; then
    ok "Device already exists (idempotent)"
  else
    fail "Device registration failed: HTTP $HTTP_CODE — $BODY"
  fi
fi

# --- 2. Create E2E encryption key ---
step 2 "Creating E2E encryption key for $IMEI"
# Check if key already exists
EXISTING_KEYS=$(curl -s "$HUB_URL/api/devices/$IMEI/keys" -H "$AUTH" 2>/dev/null || echo "[]")
HAS_KEY=$(echo "$EXISTING_KEYS" | python3 -c "
import sys, json
try:
    keys = json.load(sys.stdin)
    print('yes' if len(keys) > 0 else 'no')
except: print('no')
" 2>/dev/null)

if [[ "$HAS_KEY" == "yes" ]]; then
  ok "Key already exists (idempotent — use the key from the original creation)"
else
  RESP=$(curl -s -w "\n%{http_code}" -X POST "$HUB_URL/api/devices/$IMEI/keys" \
    -H "$AUTH" -H "$CT" \
    -d '{"mode":"decrypt"}')
  HTTP_CODE=$(echo "$RESP" | tail -1)
  BODY=$(echo "$RESP" | sed '$d')
  if [[ "$HTTP_CODE" == "201" ]]; then
    KEY_HEX=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('key_hex',''))" 2>/dev/null || echo "")
    ok "Key created"
    echo ""
    echo "  ┌─────────────────────────────────────────────────────────────────────┐"
    echo "  │ COPY THIS KEY TO ANDROID SETTINGS → ENCRYPTION → DEVICE KEY:       │"
    echo "  │                                                                     │"
    echo "  │  $KEY_HEX  │"
    echo "  │                                                                     │"
    echo "  │ ⚠  This key is shown ONCE. Save it now.                            │"
    echo "  └─────────────────────────────────────────────────────────────────────┘"
    echo ""
  else
    fail "Key creation failed: HTTP $HTTP_CODE — $BODY"
  fi
fi

# --- 3. Create routing rule: iridium → email ---
step 3 "Creating routing rule: iridium → email ($EMAIL)"
# Check if a matching route already exists
EXISTING_ROUTES=$(curl -s "$HUB_URL/api/routes" -H "$AUTH" 2>/dev/null || echo "[]")
ROUTE_EXISTS=$(echo "$EXISTING_ROUTES" | python3 -c "
import sys, json
try:
    routes = json.load(sys.stdin)
    for r in routes:
        if r.get('destination_type') == 'email' and r.get('filter') == '$EMAIL':
            print('yes'); sys.exit()
    print('no')
except: print('no')
" 2>/dev/null)

if [[ "$ROUTE_EXISTS" == "yes" ]]; then
  ok "Route already exists (idempotent)"
else
  RESP=$(curl -s -w "\n%{http_code}" -X POST "$HUB_URL/api/routes" \
    -H "$AUTH" -H "$CT" \
    -d "{\"name\":\"Demo: Iridium to Email\",\"source_type\":\"iridium\",\"destination_type\":\"email\",\"filter\":\"$EMAIL\",\"enabled\":true}")
  HTTP_CODE=$(echo "$RESP" | tail -1)
  if [[ "$HTTP_CODE" == "201" ]]; then
    ok "Route created"
  else
    fail "Route creation failed: HTTP $HTTP_CODE"
  fi
fi

# --- 4. Create SOS escalation chain ---
step 4 "Creating SOS escalation chain with email target"
# Check if a Demo SOS chain already exists
EXISTING_CHAINS=$(curl -s "$HUB_URL/api/escalation/chains" -H "$AUTH" 2>/dev/null || echo "[]")
CHAIN_EXISTS=$(echo "$EXISTING_CHAINS" | python3 -c "
import sys, json
try:
    chains = json.load(sys.stdin)
    for c in chains:
        if c.get('name') == 'Demo SOS Chain':
            print(c.get('id', '')); sys.exit()
    print('')
except: print('')
" 2>/dev/null)

if [[ -n "$CHAIN_EXISTS" ]]; then
  ok "Escalation chain already exists: $CHAIN_EXISTS (idempotent)"
  CHAIN_ID="$CHAIN_EXISTS"
else
  RESP=$(curl -s -w "\n%{http_code}" -X POST "$HUB_URL/api/escalation/chains" \
    -H "$AUTH" -H "$CT" \
    -d "{\"name\":\"Demo SOS Chain\",\"tiers\":[{\"name\":\"email_operator\",\"targets\":[\"mailto://$EMAIL\"],\"wait_sec\":30,\"max_retries\":3}]}")
  HTTP_CODE=$(echo "$RESP" | tail -1)
  BODY=$(echo "$RESP" | sed '$d')
  if [[ "$HTTP_CODE" == "201" ]]; then
    CHAIN_ID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
    ok "Escalation chain created: $CHAIN_ID"
  else
    fail "Escalation chain creation failed: HTTP $HTTP_CODE"
    CHAIN_ID=""
  fi
fi
if [[ -n "${CHAIN_ID:-}" ]]; then
  echo "  Set HUB_SOS_CHAIN_ID=$CHAIN_ID for SOS auto-trigger"
fi

# --- 5. Import recipient PGP key ---
if [[ -n "$PGP_KEY_FILE" && -f "$PGP_KEY_FILE" ]]; then
  step 5 "Importing PGP public key for $EMAIL"
  PGP_JSON=$(python3 -c "import sys,json; print(json.dumps(sys.stdin.read()))" < "$PGP_KEY_FILE")
  RESP=$(curl -s -w "\n%{http_code}" -X POST "$HUB_URL/api/email/keys" \
    -H "$AUTH" -H "$CT" \
    -d "{\"email\":\"$EMAIL\",\"armored_key\":$PGP_JSON}")
  HTTP_CODE=$(echo "$RESP" | tail -1)
  if [[ "$HTTP_CODE" == "201" ]]; then
    ok "PGP key imported for $EMAIL"
  elif [[ "$HTTP_CODE" == "409" ]]; then
    ok "PGP key already imported (idempotent)"
  else
    fail "PGP import failed: HTTP $HTTP_CODE"
  fi
else
  step 5 "Skipping PGP import (no --pgp-key provided or file not found)"
  echo "  To add later: $0 --hub-url $HUB_URL --token TOKEN --imei $IMEI --email $EMAIL --pgp-key /path/to/key.asc"
fi

# --- 6. Export Hub PGP public key ---
step 6 "Exporting Hub PGP public key"
HUB_PUBKEY_FILE="/tmp/meshsat-hub-pubkey.asc"
curl -s "$HUB_URL/api/email/keys/public" -H "$AUTH" -o "$HUB_PUBKEY_FILE" 2>/dev/null
if [[ -s "$HUB_PUBKEY_FILE" ]] && grep -q "BEGIN PGP" "$HUB_PUBKEY_FILE" 2>/dev/null; then
  ok "Hub PGP pubkey saved to $HUB_PUBKEY_FILE"
  echo "  Import into Thunderbird: Settings → End-to-End Encryption → OpenPGP Key Manager → Import"
else
  fail "Hub PGP pubkey not available (email gateway may not be enabled)"
  echo "  Ensure HUB_EMAIL_ENABLED=true and restart Hub"
fi

# --- Summary ---
echo ""
echo "═══════════════════════════════════════════════════════════════"
echo "  DEMO SETUP COMPLETE"
echo "═══════════════════════════════════════════════════════════════"
echo "  Device:      $IMEI (registered on Hub)"
echo "  Route:       iridium → email ($EMAIL)"
echo "  Escalation:  SOS → email ($EMAIL)"
echo "  Hub PGP:     $HUB_PUBKEY_FILE (import into Thunderbird)"
echo ""
echo "  NEXT STEPS:"
echo "  1. Copy the AES-256 key above into Android → Settings → Encryption"
echo "  2. Import $HUB_PUBKEY_FILE into Thunderbird"
echo "  3. Send a test message from Android via Iridium"
echo "  4. Check Hub dashboard + Thunderbird inbox"
echo "═══════════════════════════════════════════════════════════════"
