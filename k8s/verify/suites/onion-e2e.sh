#!/usr/bin/env bash
# Prove the Hub's .onion is reachable THROUGH THE TOR NETWORK (MESHSAT-1121).
#
# Everything else about Tor can look healthy while the hidden service is
# unreachable: the pod runs, the hostname file exists, tor says Bootstrapped
# 100%, and /api/tor/onion happily reports an address. None of that exercises the
# introduction points or a rendezvous. This does.
#
# It runs a throwaway Tor CLIENT in the cluster. Same cluster is not a shortcut:
# a .onion is resolved through the Tor network wherever the client sits, so the
# request still leaves, finds the intro points and rendezvous-es back.
#
# Costs nothing and touches no customer data: it fetches /healthz.
set -euo pipefail

NS=(--context notrf01 -n meshsat-hub)
POD=onion-probe-$$

onion=$(kubectl "${NS[@]}" exec tor-0 -- cat /var/lib/tor/hidden_service/hostname | tr -d '\r\n')
[ -n "$onion" ] || { echo "FAIL: tor-0 has no hostname file"; exit 1; }
echo "target: $onion"

cleanup() { kubectl "${NS[@]}" delete pod "$POD" --wait=false >/dev/null 2>&1 || true; }
trap cleanup EXIT

kubectl "${NS[@]}" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $POD, namespace: meshsat-hub}
spec:
  restartPolicy: Never
  nodeSelector: {node-role.kubernetes.io/control-plane: ""}
  tolerations:
    - {key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}
  containers:
    - {name: torclient, image: "docker.io/osminogin/tor-simple"}
    - {name: probe, image: "docker.io/curlimages/curl", command: ["sh","-c","sleep 900"]}
YAML

kubectl "${NS[@]}" wait --for=condition=Ready "pod/$POD" --timeout=180s >/dev/null

# The client needs a moment to build circuits after the SOCKS port opens.
# Retry rather than sleep a guessed amount: a first fetch often precedes a
# usable circuit, and a single failure here would say nothing.
for i in $(seq 1 10); do
  code=$(kubectl "${NS[@]}" exec "$POD" -c probe -- \
    curl -s -o /dev/null -w '%{http_code}' --max-time 60 \
    --socks5-hostname 127.0.0.1:9050 "http://$onion/healthz" 2>/dev/null || echo 000)
  if [ "$code" = "200" ]; then
    echo "PASS  the hidden service answered HTTP 200 through Tor (attempt $i)"
    exit 0
  fi
  echo "  attempt $i: $code (circuits may still be building)"
  sleep 20
done

echo "FAIL  the .onion never answered. tor-0 may be healthy and still unreachable:"
echo "      check that its descriptor is published and that hub:6070 is up."
exit 1
