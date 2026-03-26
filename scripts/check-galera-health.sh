#!/usr/bin/env bash
# check-galera-health.sh — Pre-deploy Galera cluster health gate.
# Returns 0 if cluster is healthy, 1 otherwise.
# Used by Claude Code pre-deploy hook and CI/CD pipelines.

set -euo pipefail

SSH_USER="${DEPLOY_SSH_USER:-kyriakosp}"
HOSTS=("nllei01dmz01" "grskg01dmz01")
CONTAINER_NAME="${GALERA_CONTAINER:-meshsat-mariadb}"
GARBD_CONTAINER="${GALERA_GARBD_CONTAINER:-meshsat-garbd}"
GARBD_HOST="nllei01dmz01"
DB_ROOT_PASS="${MARIADB_ROOT_PASSWORD:-}"
MIN_CLUSTER_SIZE=3
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

    # Check for bare gcomm:// in running container (incident 7/8 root cause)
    CONTAINER_CMD=$(ssh -l "${SSH_USER}" "${host}" "docker inspect ${CONTAINER_NAME} --format '{{range .Config.Cmd}}{{.}} {{end}}'" 2>/dev/null) || true
    if echo "${CONTAINER_CMD}" | grep -qE 'wsrep-cluster-address=gcomm://$|wsrep-cluster-address=gcomm:// '; then
        echo "  FAIL: Running container has BARE gcomm:// in args (stale bootstrap!)"
        echo "  FIX:  Restore .env, then: docker compose up -d mariadb (recreates container)"
        HEALTHY=false
    fi

    # Check .env for bare gcomm:// (incident 2/3/5 root cause)
    ENV_ADDR=$(ssh -l "${SSH_USER}" "${host}" "grep WSREP_CLUSTER_ADDRESS /srv/meshsat-hub/.env" 2>/dev/null) || true
    if echo "${ENV_ADDR}" | grep -qE 'WSREP_CLUSTER_ADDRESS=gcomm://$'; then
        echo "  FAIL: .env has bare gcomm:// (will cause split-brain on next recreate!)"
        HEALTHY=false
    else
        echo "  .env: ${ENV_ADDR}"
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
        echo "  FAIL: cluster_size=${CLUSTER_SIZE} < ${MIN_CLUSTER_SIZE} (split-brain!)"
        HEALTHY=false
    fi

    if [[ "${STATE_COMMENT}" != "Synced" ]]; then
        echo "  WARN: node state is ${STATE_COMMENT} (expected Synced)"
    fi

    # Check garbd on its designated host
    if [[ "${host}" == "${GARBD_HOST}" ]]; then
        echo ""
        echo "  --- garbd (${GARBD_CONTAINER}) ---"
        GARBD_RUNNING=$(ssh -l "${SSH_USER}" "${host}" "docker ps --filter name=${GARBD_CONTAINER} --filter status=running --format '{{.Names}}'" 2>/dev/null) || true
        if [[ -z "${GARBD_RUNNING}" ]]; then
            echo "  FAIL: garbd not running on ${host} — cluster has NO quorum voter!"
            echo "  FIX:  docker compose up -d garbd"
            HEALTHY=false
        else
            echo "  garbd: running"
        fi
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
