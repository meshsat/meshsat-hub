#!/usr/bin/env bash
# Runs the MeshSat authentik scripts on the shared authentik (namespace
# omoikane, notrf01) through `ak shell` on the worker pod, and stores the
# OIDC client in OpenBao for the Hub ExternalSecret (MESHSAT-936).
#
#   ./run-bootstrap.sh bootstrap                  # idempotent; writes HUB_OIDC_CLIENT_ID/SECRET to OpenBao
#   ./run-bootstrap.sh approve <email> <role>     # owner|operator|viewer
#   ./run-bootstrap.sh reject  <email>
#
# Needs: kubectl context notrf01; for `bootstrap` also bao (BAO_ADDR + token).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CTX="${KUBE_CONTEXT:-notrf01}"
NS="${AUTHENTIK_NAMESPACE:-omoikane}"
DEPLOY="${AUTHENTIK_DEPLOY:-deploy/auth-worker}"
WEBHOOK_URL="${MESHSAT_SIGNUP_WEBHOOK:-https://n8n.nuclearlighters.net/webhook/meshsat-signup}"

# Pipes a python program (stdin) into `ak shell -c` and strips authentik's
# JSON log lines and the shell banner from the output.
ak() {
  local code
  code="$(cat)"
  # grep -v exits 1 when it filters every line; that must not trip pipefail.
  kubectl --context "$CTX" -n "$NS" exec -i "$DEPLOY" -- ak shell -c "$code" 2>&1 \
    | { grep -v '^{"event"' || true; } | { grep -v '^###' || true; } | { grep -v 'objects imported automatically' || true; }
}

# Emits `NAME = <python literal>` lines for the values the scripts read.
prelude() {
  python3 - "$@" <<'PY'
import json, sys
args = sys.argv[1:]
for kv in args:
    name, _, value = kv.partition("=")
    if value.startswith("@"):
        value = open(value[1:]).read()
        print(f"{name} = {value!r}")
    elif value.startswith("json:"):
        print(f"{name} = {json.loads(value[5:])!r}")
    else:
        print(f"{name} = {value!r}")
PY
}

case "${1:-}" in
  bootstrap)
    out="$( { prelude "MESHSAT_CSS=@$HERE/meshsat-login.css" "WEBHOOK_URL=$WEBHOOK_URL"; cat "$HERE/bootstrap-meshsat.py"; } | ak )"
    echo "$out" | sed -n '/---MESHSAT_BOOTSTRAP_LOG---/,/---MESHSAT_OIDC_CONFIG---/p' | grep -v '^---'
    cid="$(echo "$out" | sed -n 's/^HUB_OIDC_CLIENT_ID=//p')"
    csec="$(echo "$out" | sed -n 's/^HUB_OIDC_CLIENT_SECRET=//p')"
    if [ -z "$cid" ] || [ -z "$csec" ]; then echo "FATAL: no OIDC config in output"; echo "$out" | tail -25; exit 1; fi
    tmp="$(mktemp)"; chmod 600 "$tmp"
    printf '{"HUB_OIDC_CLIENT_ID":"%s","HUB_OIDC_CLIENT_SECRET":"%s"}' "$cid" "$csec" > "$tmp"
    bao kv patch -mount=secret ci-no/apps/meshsat-hub/hub @"$tmp" >/dev/null
    shred -u "$tmp"
    echo "OIDC client stored in OpenBao ci-no/apps/meshsat-hub/hub (client id ${cid:0:6}...)"
    echo "$out" | sed -n 's/^ISSUER=/issuer:     /p; s/^ENROLLMENT=/enrollment: /p'
    ;;
  approve|reject)
    action="$1"; shift
    [ -n "${1:-}" ] || { echo "usage: $0 $action <email> [role]"; exit 2; }
    args="$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1:]))' "$action" "$@")"
    { prelude "ARGS=json:$args"; cat "$HERE/approve-meshsat-user.py"; } | ak
    ;;
  *)
    echo "usage: $0 bootstrap | approve <email> <owner|operator|viewer> | reject <email>"; exit 2;;
esac
