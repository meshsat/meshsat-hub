#!/usr/bin/env bash
# Runs the MeshSat authentik scripts on the shared authentik (namespace
# omoikane, notrf01) through `ak shell` on the worker pod, and stores the
# OIDC client in OpenBao for the Hub ExternalSecret (MESHSAT-936).
#
#   ./run-bootstrap.sh bootstrap                  # idempotent; writes HUB_OIDC_CLIENT_ID/SECRET to OpenBao
#   ./run-bootstrap.sh approve <email> <role>     # owner|operator|viewer
#   ./run-bootstrap.sh reject  <email>
#   ./run-bootstrap.sh pause                      # stop accepting access requests
#   ./run-bootstrap.sh resume
#
# `bootstrap` optionally reads TURNSTILE_SITE_KEY and TURNSTILE_SECRET from the
# environment; without them the CAPTCHA stage is skipped rather than half-built.
# The live pair is in OpenBao, so a re-run keeps the CAPTCHA rather than leaving
# it on whatever it had:
#
#   eval "$(bao kv get -mount=secret -format=json ci-no/apps/meshsat-hub/turnstile \
#           | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]["data"]; \
#             print("export TURNSTILE_SITE_KEY=%s TURNSTILE_SECRET=%s" % (d["TURNSTILE_SITE_KEY"], d["TURNSTILE_SECRET"]))')"
#
# The Cloudflare widget is "meshsat-hub-enrollment", scoped to auth.meshsat.net.
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

# The EmailStage renders INLINE IN THE FLOW EXECUTOR, which runs in auth-server
# and not in the worker this script otherwise talks to. A template mounted only
# on the worker therefore passes every check here and still breaks every real
# signup with TemplateDoesNotExist, after the account row has already been
# written (OMOIKANE-1664, 2026-09-10 — 25 minutes of a public signup page
# answering with an error page). So ask the server itself, every time.
verify_email_templates() {
  local server="${AUTHENTIK_SERVER_DEPLOY:-deploy/auth-server}" bad=0
  for tpl in email/meshsat_account_confirmation.html email/meshsat_password_reset.html; do
    if kubectl --context "$CTX" -n "$NS" exec -i "$server" -- \
         ak shell -c "from django.template.loader import get_template; get_template('$tpl'); print('OK')" 2>&1 \
         | grep -q '^OK$'; then
      echo "template $tpl is readable by the flow executor"
    else
      echo "FATAL: $tpl is NOT readable by auth-server, which is what renders it."
      echo "       Enrollment will fail after the details stage. Mount the ConfigMap"
      echo "       authentik-meshsat-email-templates at /templates/email on the SERVER"
      echo "       deployment (omoikane k8s/auth/deployment-server.yaml), not only the worker."
      bad=1
    fi
  done
  [ "$bad" -eq 0 ] || exit 1
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
    out="$( { prelude "MESHSAT_CSS=@$HERE/meshsat-login.css" "WEBHOOK_URL=$WEBHOOK_URL" \
                      "DISPOSABLE_DOMAINS=@$HERE/disposable-domains.txt" \
                      "TURNSTILE_SITE_KEY=${TURNSTILE_SITE_KEY:-}" "TURNSTILE_SECRET=${TURNSTILE_SECRET:-}"; \
              cat "$HERE/bootstrap-meshsat.py"; } | ak )"
    echo "$out" | sed -n '/---MESHSAT_BOOTSTRAP_LOG---/,/---MESHSAT_OIDC_CONFIG---/p' | grep -v '^---'
    cid="$(echo "$out" | sed -n 's/^HUB_OIDC_CLIENT_ID=//p')"
    csec="$(echo "$out" | sed -n 's/^HUB_OIDC_CLIENT_SECRET=//p')"
    if [ -z "$cid" ] || [ -z "$csec" ]; then echo "FATAL: no OIDC config in output"; echo "$out" | tail -25; exit 1; fi
    tmp="$(mktemp)"; chmod 600 "$tmp"
    atok="$(echo "$out" | sed -n 's/^HUB_AUTHENTIK_TOKEN=//p')"
    if [ -n "$atok" ]; then
      printf '{"HUB_OIDC_CLIENT_ID":"%s","HUB_OIDC_CLIENT_SECRET":"%s","HUB_AUTHENTIK_TOKEN":"%s"}' "$cid" "$csec" "$atok" > "$tmp"
    else
      printf '{"HUB_OIDC_CLIENT_ID":"%s","HUB_OIDC_CLIENT_SECRET":"%s"}' "$cid" "$csec" > "$tmp"
    fi
    bao kv patch -mount=secret ci-no/apps/meshsat-hub/hub @"$tmp" >/dev/null
    shred -u "$tmp"
    echo "OIDC client stored in OpenBao ci-no/apps/meshsat-hub/hub (client id ${cid:0:6}...)"
    echo "$out" | sed -n 's/^ISSUER=/issuer:     /p; s/^ENROLLMENT=/enrollment: /p'
    verify_email_templates
    ;;
  pause|resume)
    # Enrollment on or off: one boolean on the pause policy's binding. The
    # policy itself is created by `bootstrap`; this only flips whether the flow
    # consults it.
    want="$([ "$1" = pause ] && echo True || echo False)"
    { prelude "WANT=$want"; cat <<'PY'; } | ak
from authentik.flows.models import Flow
from authentik.policies.models import PolicyBinding
from authentik.policies.expression.models import ExpressionPolicy
want = WANT == "True"
flow = Flow.objects.filter(slug="meshsat-enrollment").first()
pol = ExpressionPolicy.objects.filter(name="meshsat-enrollment-paused").first()
b = PolicyBinding.objects.filter(policy=pol, target=flow).first() if (flow and pol) else None
if not b:
    print("no pause binding yet; run `run-bootstrap.sh bootstrap` first")
elif b.enabled == want:
    print("signups are already " + ("PAUSED" if want else "OPEN"))
else:
    b.enabled = want
    b.save()
    print("signups are now " + ("PAUSED" if want else "OPEN"))
PY
    ;;
  approve|reject)
    action="$1"; shift
    [ -n "${1:-}" ] || { echo "usage: $0 $action <email> [role]"; exit 2; }
    args="$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1:]))' "$action" "$@")"
    { prelude "ARGS=json:$args"; cat "$HERE/approve-meshsat-user.py"; } | ak
    ;;
  *)
    echo "usage: $0 bootstrap | pause | resume | approve <email> <owner|operator|viewer> | reject <email>"; exit 2;;
esac
