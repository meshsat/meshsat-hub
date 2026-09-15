#!/bin/bash
# Nightly production verification (MESHSAT-1153). Runs the read-only live
# suites against the deployed Hub from inside the cluster, in dependency
# order, and reports: a PASS beats the hidden `verify` HEARTBEAT monitor on
# status.meshsat.net (silence for 26 h shows DEGRADED there and pages the
# operators' ntfy topic through the page's own alert), a FAIL pushes the
# failing suite names to that topic directly and leaves the Job Failed.
#
# Nothing here moves money or issues a document: stripe-suite, refund-suite,
# mail-probe and vat-suite are deliberately not in the list.
set -o pipefail
export VERIFY_IN_CLUSTER=1
export PATH="/suites:$PATH"
export HOME=/tmp KUBECACHEDIR=/tmp/kube
cd /suites || exit 2

suites=(
  deployed-is-pinned.sh
  capabilities-suite.py
  position-dedup-suite.py
  integrations-replica-suite.py
  quota-suite.py
  notify-e2e.py
  journey-suite.py
  onion-e2e.sh
  status-page-suite.py
)

# Initialised empty on purpose: under `set -u` an array that was only
# declared reads as unbound the first time nothing failed, which is exactly
# the run that must report PASS and beat the heartbeat.
failed=()
passed=()
start=$(date +%s)

# The restore drill (docs/restore-drill.md) is run by hand every quarter and
# its date is recorded in the hub-verify-state ConfigMap. Older than 100 days
# is a failure: a backup nobody has restored is a belief, not a backup.
last="${RESTORE_DRILL_LAST:-}"
if [ -z "$last" ]; then
  echo "FAIL  restore-drill-age: no drill recorded (RESTORE_DRILL_LAST unset)"; failed+=("restore-drill-age")
else
  age=$(( ( $(date +%s) - $(date -d "$last" +%s 2>/dev/null || echo 0) ) / 86400 ))
  if [ "$age" -gt 100 ]; then
    echo "FAIL  restore-drill-age: last drill $last is $age days old (limit 100)"; failed+=("restore-drill-age")
  else
    echo "PASS  restore-drill-age: last drill $last, $age days ago"; passed+=("restore-drill-age")
  fi
fi

for s in "${suites[@]}"; do
  echo "=================== $s"
  case "$s" in
    *.py) runner=(python3 "$s") ;;
    *)    runner=(bash "$s") ;;
  esac
  if timeout 900 "${runner[@]}" > "/tmp/$s.log" 2>&1; then
    tail -n 3 "/tmp/$s.log"; echo "PASS  $s"; passed+=("$s")
  else
    rc=$?; tail -n 40 "/tmp/$s.log"; echo "FAIL  $s (exit $rc)"; failed+=("$s")
  fi
done

if [ "${VERIFY_FORCE_FAIL:-0}" = "1" ]; then
  echo "FAIL  forced (VERIFY_FORCE_FAIL=1, the reporting drill)"; failed+=("forced")
fi

took=$(( $(date +%s) - start ))
echo "==================="
echo "passed: ${#passed[@]}  failed: ${#failed[@]}  in ${took}s"

if [ "${#failed[@]}" -eq 0 ]; then
  if [ -n "${VERIFY_HEARTBEAT_SECRET:-}" ]; then
    curl -fsS --max-time 20 -o /dev/null "https://status.meshsat.net/ext/heartbeat/verify/${VERIFY_HEARTBEAT_SECRET}" \
      && echo "heartbeat sent to status.meshsat.net" || echo "WARN  heartbeat not delivered"
  fi
  echo "VERIFY PASS"
  exit 0
fi

msg="hub-verify FAILED (${#failed[@]}): ${failed[*]} -- passed ${#passed[@]}. kubectl -n meshsat-hub logs job/<name>"
if [ -n "${NTFY_URL:-}" ] && [ -n "${NTFY_TOPIC:-}" ]; then
  curl -fsS --max-time 20 -o /dev/null -H "Title: MeshSat Hub nightly verification failed" -H "Priority: high" -H "Tags: rotating_light" \
    ${NTFY_TOKEN:+-H "Authorization: Bearer ${NTFY_TOKEN}"} -d "$msg" "${NTFY_URL%/}/${NTFY_TOPIC}" \
    && echo "ntfy sent" || echo "WARN  ntfy not delivered"
fi
echo "VERIFY FAIL: ${failed[*]}"
exit 1
