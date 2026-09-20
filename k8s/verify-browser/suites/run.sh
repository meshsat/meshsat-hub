#!/bin/bash
# Nightly real-browser verification (MESHSAT-1224). Deliberately a SEPARATE
# CronJob from hub-verify: that one runs on a 40 MB alpine image and passes
# 10/10 today, and a browser needs a ~2 GB Debian image that Playwright
# actually supports. Keeping them apart means a browser problem cannot take
# the main nightly down with it.
#
# Reporting matches hub-verify: a FAIL pushes the failing suite names to the
# operators' ntfy topic and leaves the Job Failed. There is no heartbeat here
# -- status.meshsat.net's `verify` monitor belongs to hub-verify, and a second
# writer to it would hide that one going silent.
set -o pipefail
export HOME=/tmp XDG_CACHE_HOME=/tmp/.cache XDG_CONFIG_HOME=/tmp/.config
cd /suites || exit 2

start=$(date +%s)
echo "=================== browser-suite.py"
if timeout 600 python3 browser-suite.py; then
  rc=0
else
  rc=$?
fi
echo "==================="
echo "browser-suite exit $rc in $(( $(date +%s) - start ))s"

if [ "$rc" -eq 0 ]; then
  echo "BROWSER VERIFY PASS"
  exit 0
fi

msg="hub-browser-verify FAILED: a real browser found a CSP violation or a broken CAPTCHA frame on auth.meshsat.net / hub.meshsat.net. kubectl -n meshsat-hub logs job/<name>"
if [ -n "${NTFY_URL:-}" ] && [ -n "${NTFY_TOPIC:-}" ]; then
  curl -fsS --max-time 20 -o /dev/null \
    -H "Title: MeshSat sign-up or SPA broken in a real browser" \
    -H "Priority: high" -H "Tags: rotating_light" \
    ${NTFY_TOKEN:+-H "Authorization: Bearer ${NTFY_TOKEN}"} \
    -d "$msg" "${NTFY_URL%/}/${NTFY_TOPIC}" \
    && echo "ntfy sent" || echo "WARN  ntfy not delivered"
fi
echo "BROWSER VERIFY FAIL"
exit 1
