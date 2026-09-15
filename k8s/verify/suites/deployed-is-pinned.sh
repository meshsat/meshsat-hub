#!/usr/bin/env bash
# Is the Hub the cluster is RUNNING the one the repo PINS?
#
# Run this before measuring anything on production. A green pipeline is not a
# deployed binary: bump_k8s_pin writes the digest in a SEPARATE [skip ci] commit
# after the code commit, so Argo reports Synced/Healthy while still running the
# previous image. Pod age, restart counts and log lines all lie about this --
# a log line only identifies a build if that line changed in that very commit.
#
# I tested the wrong binary four times in one session for want of this, once
# reporting 5/5 FAIL and nearly declaring a correct fix broken.
set -euo pipefail

NS=(--context notrf01 -n meshsat-hub)
if [ "${VERIFY_IN_CLUSTER:-0}" != "1" ]; then
  cd "$(dirname "$0")/../../.."
  git fetch -q origin main
fi

# Only the 64 hex characters. The pin line carries a trailing comment, and a
# naive cut captures it -- which broke the first version of this check while a
# truncated display hid the difference.
# No `grep -m1`: it exits on the first match, the producer takes SIGPIPE once
# its output outgrows a pipe buffer, and under pipefail that is exit 141 -- the
# trap scripts/check-pipefail-sigpipe.sh exists to catch, and it caught this
# very line in pipeline 54000. `sed -n 1p` reads to EOF.
if [ "${VERIFY_IN_CLUSTER:-0}" = "1" ]; then
  # Inside the cluster there is no git checkout. The pin is what Argo applied
  # to the Deployment; "deployed" is what every hub pod actually pulled
  # (imageID), the comparison that catches a pod still on the previous digest.
  want=$(kubectl "${NS[@]}" get deploy hub -o jsonpath='{.spec.template.spec.containers[0].image}' \
         | sed -n 's/.*@sha256:\([0-9a-f]\{64\}\).*/\1/p')
  have=$(kubectl "${NS[@]}" get pods -l app.kubernetes.io/name=hub \
         -o jsonpath='{range .items[*]}{.status.containerStatuses[?(@.name=="hub")].imageID}{"\n"}{end}' \
         | sed -n 's/.*sha256:\([0-9a-f]\{64\}\).*/\1/p' | sort -u | tr '\n' ' ' | sed 's/ $//')
else
  want=$(git show origin/main:k8s/kustomization.yaml \
         | grep 'digest:' | sed -n 's/.*sha256:\([0-9a-f]\{64\}\).*/\1/p' | sed -n 1p)
  have=$(kubectl "${NS[@]}" get deploy hub \
         -o jsonpath='{.spec.template.spec.containers[0].image}' \
         | sed -n 's/.*@sha256:\([0-9a-f]\{64\}\).*/\1/p')
fi

echo "pinned:   ${want:-<none>}"
echo "deployed: ${have:-<none>}"

if [ -z "$want" ] || [ -z "$have" ]; then
  echo "FAIL  could not read one of the digests"; exit 1
fi
if [ "$want" != "$have" ]; then
  echo "NOT YET  production is running an older image; anything measured now is about THAT image"
  exit 1
fi

upd=$(kubectl "${NS[@]}" get deploy hub -o jsonpath='{.status.updatedReplicas}')
rdy=$(kubectl "${NS[@]}" get deploy hub -o jsonpath='{.status.readyReplicas}')
tot=$(kubectl "${NS[@]}" get deploy hub -o jsonpath='{.status.replicas}')
rep=$(kubectl "${NS[@]}" get deploy hub -o jsonpath='{.spec.replicas}')
echo "updated=${upd:-0}/${rep:-?} ready=${rdy:-0}/${rep:-?} total=${tot:-0}/${rep:-?}"
# updated==ready==spec is NOT enough: mid-rollout, updated=2 (both new pods
# exist) and ready=2 (one new + one OLD still ready) both hold while a new pod
# is Pending and an old one is Terminating. This passed in exactly that state
# on 2026-09-14 and a probe went out against a mixed fleet. status.replicas
# counts old pods too, so it must ALSO equal spec: no surplus old pod left.
[ "${upd:-0}" = "${rep:-x}" ] && [ "${rdy:-0}" = "${rep:-x}" ] && [ "${tot:-0}" = "${rep:-x}" ] \
  && { echo "OK  the deployed image is the pinned one and every replica is on it"; exit 0; }
echo "NOT YET  the rollout is still in progress"; exit 1
