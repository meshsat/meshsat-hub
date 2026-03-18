#!/usr/bin/env bash
# check-galera-health.sh — Pre-deploy Galera cluster health gate.
# Returns 0 if cluster is healthy, 1 otherwise.
# Used by Claude Code pre-deploy hook and CI/CD pipelines.

set -euo pipefail

HOSTS=("nllei01dmz01" "grskg01dmz01")
MIN_CLUSTER_SIZE=2
HEALTHY=true

echo "=== Galera Cluster Health Check ==="

for host in "${HOSTS[@]}"; do
    echo ""
    echo "--- ${host} ---"

    # Check SSH reachability
    if ! ssh -o ConnectTimeout=5 -o BatchMode=yes "${host}" "true" 2>/dev/null; then
        echo "  FAIL: ${host} unreachable via SSH"
        HEALTHY=false
        continue
    fi

    # Query Galera status
    STATUS=$(ssh "${host}" "docker exec meshsat-hub-mariadb-1 mariadb -u root -p\"\${MARIADB_ROOT_PASSWORD}\" -N -e \"SELECT VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME IN ('wsrep_cluster_size','wsrep_ready','wsrep_local_state_comment') ORDER BY VARIABLE_NAME\" 2>/dev/null" 2>/dev/null) || {
        echo "  FAIL: Could not query MariaDB on ${host}"
        HEALTHY=false
        continue
    }

    # Parse results (alphabetical: wsrep_cluster_size, wsrep_local_state_comment, wsrep_ready)
    CLUSTER_SIZE=$(echo "${STATUS}" | sed -n '1p')
    STATE_COMMENT=$(echo "${STATUS}" | sed -n '2p')
    READY=$(echo "${STATUS}" | sed -n '3p')

    echo "  wsrep_cluster_size:         ${CLUSTER_SIZE}"
    echo "  wsrep_ready:                ${READY}"
    echo "  wsrep_local_state_comment:  ${STATE_COMMENT}"

    if [[ "${READY}" != "ON" ]]; then
        echo "  FAIL: wsrep_ready is not ON"
        HEALTHY=false
    fi

    if [[ "${CLUSTER_SIZE}" -lt "${MIN_CLUSTER_SIZE}" ]]; then
        echo "  FAIL: cluster size ${CLUSTER_SIZE} < ${MIN_CLUSTER_SIZE}"
        HEALTHY=false
    fi

    if [[ "${STATE_COMMENT}" != "Synced" ]]; then
        echo "  WARN: node state is ${STATE_COMMENT} (expected Synced)"
    fi
done

echo ""
if [[ "${HEALTHY}" == "true" ]]; then
    echo "=== PASS: Cluster is healthy, safe to deploy ==="
    exit 0
else
    echo "=== FAIL: Cluster is NOT healthy — DO NOT DEPLOY ==="
    exit 1
fi
