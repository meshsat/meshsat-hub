#!/usr/bin/env bash
# One-off Job: the pinned Hub image with --migrate-only against a CNPG rw Service,
# creating the Postgres schema (MESHSAT-944). Usage: job-migrate.sh <rw-service> [namespace-db]
set -euo pipefail
RW="${1:?rw service, e.g. meshsat-hub-drill-rw or meshsat-hub-main-rw}"
NSDB="${2:-meshsat-hub-db}"
CTX="${KUBE_CONTEXT:-notrf01}"
K="kubectl --context $CTX -n meshsat-hub"
IMAGE="$($K get deploy hub -o jsonpath='{.spec.template.spec.containers[0].image}')"
NAME="hub-migrate-$(date -u +%H%M%S)"
$K delete job -l app.kubernetes.io/name=hub-migrate --ignore-not-found >/dev/null
cat <<YAML | $K apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: $NAME
  labels: {app.kubernetes.io/name: hub-migrate}
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 3600
  template:
    metadata:
      labels: {app.kubernetes.io/name: hub-migrate}
    spec:
      restartPolicy: Never
      serviceAccountName: meshsat-hub
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532}
      containers:
        - name: migrate
          image: $IMAGE
          # hub-secrets carries HUB_DATABASE_URL for meshsat-hub-main-rw; point the same
          # credentials at the requested rw Service (drill cluster or main).
          command: ["sh", "-c", "export HUB_DATABASE_URL=\"\$(printf '%s' \"\$HUB_DATABASE_URL\" | sed 's/meshsat-hub-main-rw/$RW/')\"; exec meshsat-hub --migrate-only"]
          envFrom:
            - configMapRef: {name: hub-config}
            - secretRef: {name: hub-secrets}
          resources: {requests: {memory: 128Mi, cpu: 100m}, limits: {memory: 512Mi}}
YAML
echo "waiting for $NAME"
$K wait --for=condition=complete --timeout=300s job/$NAME || { $K logs job/$NAME | tail -30; exit 1; }
$K logs job/$NAME | grep -E "migrat|store ready|error" | tail -5
