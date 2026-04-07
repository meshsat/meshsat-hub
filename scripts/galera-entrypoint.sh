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
#   4. Auto-bootstraps if gvwstate.dat is missing AND remote data node is unreachable
#      (Incident 11 fix: prevents crash loop after network partition recovery)
#
# Bootstrap procedure (manual):
#   touch /var/lib/mysql/force-bootstrap   (on the host/volume)
#   docker compose up -d mariadb           (entrypoint detects flag, bootstraps)
#   # Flag is consumed — next restart joins normally. .env is NEVER modified.
#
# WSREP_CLUSTER_ADDRESS must ALWAYS be the full peer list:
#   gcomm://192.168.192.10,192.168.15.10

set -euo pipefail

BOOTSTRAP_FLAG="/var/lib/mysql/force-bootstrap"
GVWSTATE="/var/lib/mysql/gvwstate.dat"
GRASTATE="/var/lib/mysql/grastate.dat"

# Auto-bootstrap settings (Incident 11 fix)
AUTO_BOOTSTRAP_DELAY="${GALERA_AUTO_BOOTSTRAP_DELAY:-60}"   # seconds to wait before auto-bootstrap
GARBD_PORT="${GALERA_GARBD_PORT:-4570}"                     # garbd listen port (local)
REMOTE_GALERA_PORT="${GALERA_REMOTE_PORT:-4567}"            # remote MariaDB galera port

log() {
    echo "galera-entrypoint: $(date -u '+%Y-%m-%d %H:%M:%S') $*"
}

# --- SAFETY GUARD ---
# Refuse to start if any arg contains bare gcomm://
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

# --- MANUAL BOOTSTRAP (flag file) ---
if [ -f "$BOOTSTRAP_FLAG" ]; then
    log "Bootstrap flag detected at ${BOOTSTRAP_FLAG}"
    log "Adding --wsrep-new-cluster to startup args"
    log "Removing flag — next restart will join normally"
    rm -f "$BOOTSTRAP_FLAG"

    if [ -f "$GRASTATE" ]; then
        sed -i 's/safe_to_bootstrap: 0/safe_to_bootstrap: 1/' "$GRASTATE"
        log "Set safe_to_bootstrap=1 in grastate.dat"
    fi

    exec docker-entrypoint.sh "$@" --wsrep-new-cluster
fi

# --- AUTO-BOOTSTRAP (Incident 11 fix) ---
# When gvwstate.dat is missing (lost during NON_PRIM crash), pc.recovery cannot
# work. If we can reach garbd locally but NOT the remote data node, the cluster
# is partitioned and manual bootstrap would be required. Instead, we wait and
# then auto-bootstrap to avoid the crash loop that extended Incident 11 by 90min.
#
# Conditions (ALL must be true):
#   1. gvwstate.dat does NOT exist (pc.recovery will fail)
#   2. grastate.dat EXISTS (this node has data — not a fresh install)
#   3. Local garbd is reachable (we have a quorum partner)
#   4. Remote data node is NOT reachable (cluster is partitioned)
#   5. After waiting ${AUTO_BOOTSTRAP_DELAY}s, remote is STILL unreachable
if [ ! -f "$GVWSTATE" ] && [ -f "$GRASTATE" ]; then
    log "WARNING: gvwstate.dat missing — pc.recovery will fail"

    # Extract remote node address from cluster address in command args
    REMOTE_ADDR=""
    LOCAL_ADDR="${WSREP_NODE_ADDRESS:-}"
    for arg in "$@"; do
        if [[ "$arg" == --wsrep-cluster-address=* ]]; then
            CLUSTER_ADDR="${arg#*=}"
            # Parse comma-separated addresses from gcomm://addr1,addr2,addr3:port
            CLUSTER_ADDR="${CLUSTER_ADDR#gcomm://}"
            IFS=',' read -ra ADDRS <<< "$CLUSTER_ADDR"
            for addr in "${ADDRS[@]}"; do
                # Strip port suffix if present (e.g., 192.168.192.10:4570 → 192.168.192.10)
                host="${addr%%:*}"
                # Skip local address and garbd address
                if [ -n "$LOCAL_ADDR" ] && [ "$host" != "$LOCAL_ADDR" ]; then
                    REMOTE_ADDR="$host"
                    break
                fi
            done
        fi
    done

    if [ -z "$REMOTE_ADDR" ]; then
        log "Could not determine remote node address — skipping auto-bootstrap"
    else
        # Check garbd reachability (local, should be on same host or LAN)
        GARBD_REACHABLE=false
        if timeout 3 bash -c "echo >/dev/tcp/127.0.0.1/${GARBD_PORT}" 2>/dev/null; then
            GARBD_REACHABLE=true
        elif [ -n "$LOCAL_ADDR" ] && timeout 3 bash -c "echo >/dev/tcp/${LOCAL_ADDR}/${GARBD_PORT}" 2>/dev/null; then
            GARBD_REACHABLE=true
        fi

        # Check remote data node reachability
        REMOTE_REACHABLE=false
        if timeout 3 bash -c "echo >/dev/tcp/${REMOTE_ADDR}/${REMOTE_GALERA_PORT}" 2>/dev/null; then
            REMOTE_REACHABLE=true
        fi

        log "Connectivity check: garbd=${GARBD_REACHABLE} remote=${REMOTE_ADDR}:${REMOTE_GALERA_PORT}=${REMOTE_REACHABLE}"

        if [ "$GARBD_REACHABLE" = true ] && [ "$REMOTE_REACHABLE" = false ]; then
            log "Cluster partitioned: garbd reachable but remote data node unreachable"
            log "Waiting ${AUTO_BOOTSTRAP_DELAY}s before auto-bootstrap (tunnel may recover)..."
            sleep "$AUTO_BOOTSTRAP_DELAY"

            # Recheck remote after delay
            if timeout 3 bash -c "echo >/dev/tcp/${REMOTE_ADDR}/${REMOTE_GALERA_PORT}" 2>/dev/null; then
                log "Remote node recovered during delay — proceeding with normal join"
            else
                log "Remote still unreachable after ${AUTO_BOOTSTRAP_DELAY}s — AUTO-BOOTSTRAPPING"
                if [ -f "$GRASTATE" ]; then
                    sed -i 's/safe_to_bootstrap: 0/safe_to_bootstrap: 1/' "$GRASTATE"
                    log "Set safe_to_bootstrap=1 in grastate.dat"
                fi
                exec docker-entrypoint.sh "$@" --wsrep-new-cluster
            fi
        fi
    fi
fi

# --- NORMAL START ---
exec docker-entrypoint.sh "$@"
