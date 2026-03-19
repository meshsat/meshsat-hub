#!/usr/bin/env bash
# check-galera-health.sh — Pre-deploy Galera cluster health gate.
# Returns 0 if cluster is healthy, 1 otherwise.
# Used by Claude Code pre-deploy hook and CI/CD pipelines.

set -euo pipefail

SSH_USER="${DEPLOY_SSH_USER:-kyriakosp}"
HOSTS=("nllei01dmz01" "grskg01dmz01")
CONTAINER_NAME="${GALERA_CONTAINER:-meshsat-mariadb}"
DB_ROOT_PASS="${MARIADB_ROOT_PASSWORD:-}"
MIN_CLUSTER_SIZE=2
HEALTHY=true

echo "=== Galera Cluster Health Check ==="

for host in "${HOSTS[@]}"; do
    echo ""
    echo "--- ${host} ---"

    # Check SSH reachability
    if ! ssh -o ConnectTimeout=5 -o BatchMode=yes -l "${SSH_USER}" "${host}" "true" 2>/dev/null; then
        echo "  FAIL: ${host} unreachable via SSH (user: ${SSH_USER})"
        HEALTHY=false
        continue
    fi

    # Get root password from container env if not set
    if [[ -z "${DB_ROOT_PASS}" ]]; then
        DB_ROOT_PASS=$(ssh -l "${SSH_USER}" "${host}" "docker inspect ${CONTAINER_NAME} --format '{{range .Config.Env}}{{println .}}{{end}}'" 2>/dev/null | grep MARIADB_ROOT_PASSWORD | cut -d= -f2)
    fi

    # Query Galera status
    STATUS=$(ssh -l "${SSH_USER}" "${host}" "docker exec ${CONTAINER_NAME} mariadb -u root -p'${DB_ROOT_PASS}' -N -e \"SELECT VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME IN ('wsrep_cluster_size','wsrep_ready','wsrep_local_state_comment') ORDER BY VARIABLE_NAME\" 2>/dev/null" 2>/dev/null) || {
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
