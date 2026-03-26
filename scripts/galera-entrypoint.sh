#!/usr/bin/env bash
# galera-entrypoint.sh — Runtime bootstrap wrapper for MariaDB Galera.
#
# Solves the #1 cause of split-brain: Docker bakes WSREP_CLUSTER_ADDRESS into
# the container at CREATE time. If bootstrapped with bare gcomm://, the container
# keeps that stale address forever, even after .env is restored.
#
# This entrypoint:
#   1. REFUSES to start if WSREP_CLUSTER_ADDRESS is bare gcomm:// (safety guard)
#   2. Adds --wsrep-new-cluster at RUNTIME if a flag file exists (bootstrap)
#   3. Deletes the flag file after use (next restart joins normally)
#
# Bootstrap procedure:
#   touch /var/lib/mysql/force-bootstrap   (on the host/volume)
#   docker compose up -d mariadb           (entrypoint detects flag, bootstraps)
#   # Flag is consumed — next restart joins normally. .env is NEVER modified.
#
# WSREP_CLUSTER_ADDRESS must ALWAYS be the full peer list:
#   gcomm://192.168.192.10,192.168.15.10

set -euo pipefail

BOOTSTRAP_FLAG="/var/lib/mysql/force-bootstrap"

# SAFETY GUARD: Refuse to start if any arg contains bare gcomm://
# This catches the root cause of 6 out of 8 split-brain incidents.
for arg in "$@"; do
    if [[ "$arg" == "--wsrep-cluster-address=gcomm://" ]]; then
        echo "============================================================"
        echo "FATAL: Bare gcomm:// detected in --wsrep-cluster-address."
        echo ""
        echo "This WILL cause an independent bootstrap and split-brain."
        echo "The .env file must have the full cluster address:"
        echo "  WSREP_CLUSTER_ADDRESS=gcomm://192.168.192.10,192.168.15.10"
        echo ""
        echo "To bootstrap intentionally, use the flag file instead:"
        echo "  touch /var/lib/mysql/force-bootstrap"
        echo "  docker compose up -d mariadb"
        echo "============================================================"
        exit 1
    fi
done

# Check for bootstrap flag file (lives on the persistent data volume)
if [ -f "$BOOTSTRAP_FLAG" ]; then
    echo "galera-entrypoint: Bootstrap flag detected at ${BOOTSTRAP_FLAG}"
    echo "galera-entrypoint: Adding --wsrep-new-cluster to startup args"
    echo "galera-entrypoint: Removing flag — next restart will join normally"
    rm -f "$BOOTSTRAP_FLAG"

    # Ensure safe_to_bootstrap=1 in grastate.dat if it exists
    GRASTATE="/var/lib/mysql/grastate.dat"
    if [ -f "$GRASTATE" ]; then
        sed -i 's/safe_to_bootstrap: 0/safe_to_bootstrap: 1/' "$GRASTATE"
        echo "galera-entrypoint: Set safe_to_bootstrap=1 in grastate.dat"
    fi

    exec docker-entrypoint.sh "$@" --wsrep-new-cluster
fi

# Normal start: join existing cluster
exec docker-entrypoint.sh "$@"
