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
#   4. Auto-bootstraps if gvwstate.dat is missing AND:
#      a. Remote data node is unreachable (Incident 11 fix), OR
#      b. Remote is reachable but join keeps failing — no primary exists
#         (Incident 13 fix: handles "both nodes NON_PRIM with tunnel up")
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
RETRY_FILE="/var/lib/mysql/auto-bootstrap-retries"

# Auto-bootstrap settings (Incident 11 fix)
AUTO_BOOTSTRAP_DELAY="${GALERA_AUTO_BOOTSTRAP_DELAY:-60}"   # seconds to wait before auto-bootstrap
GARBD_PORT="${GALERA_GARBD_PORT:-4570}"                     # garbd listen port (local)
REMOTE_GALERA_PORT="${GALERA_REMOTE_PORT:-4567}"            # remote MariaDB galera port
MAX_JOIN_RETRIES="${GALERA_MAX_JOIN_RETRIES:-3}"            # auto-bootstrap after N failed joins

log() {
    echo "galera-entrypoint: $(date -u '+%Y-%m-%d %H:%M:%S') $*"
}

# Save script args so auto-bootstrap helpers can access them
SCRIPT_ARGS=("$@")

auto_bootstrap() {
    local reason="$1"
    log "AUTO-BOOTSTRAPPING: ${reason}"
    rm -f "$RETRY_FILE"

    # --- SEQNO SAFETY CHECK (Incident 14 fix) ---
    # If grastate.dat has seqno: -1, bootstrapping creates an EMPTY cluster.
    # Both nodes then need SST but neither has data to donate → deadlock.
    # Refuse auto-bootstrap with seqno -1 — require manual intervention.
    if [ -f "$GRASTATE" ]; then
        local current_seqno
        current_seqno=$(grep -oP 'seqno:\s+\K-?\d+' "$GRASTATE" 2>/dev/null || echo "unknown")
        if [ "$current_seqno" = "-1" ]; then
            log "REFUSING auto-bootstrap: grastate seqno is -1 (unknown/crashed state)"
            log "Bootstrapping with seqno -1 creates an empty cluster → SST deadlock"
            log "Manual intervention required: set force-bootstrap flag after verifying data"
            log "Proceeding with normal join instead (will retry)"
            return 0  # Fall through to normal start
        fi
        sed -i 's/safe_to_bootstrap: 0/safe_to_bootstrap: 1/' "$GRASTATE"
        log "Set safe_to_bootstrap=1 in grastate.dat"
    fi
    exec docker-entrypoint.sh "${SCRIPT_ARGS[@]}" --wsrep-new-cluster
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

# --- PROTO VERSION POISON GUARD (Incident 14 fix) ---
# garbd advertises proto 127/127 when it forms PRIMARY alone. If gvwstate.dat
# contains these values, joining that group will crash MariaDB with SIGSEGV.
# Detect and clean before attempting to join.
if [ -f "$GVWSTATE" ]; then
    # Check for proto 127 in the saved primary component state
    if grep -qE 'Protocols\s*:\s*2\s*/\s*127\s*/\s*127' "$GVWSTATE" 2>/dev/null; then
        log "DANGER: gvwstate.dat contains garbd proto 127/127 poison (Incident 14)"
        log "Removing poisoned gvwstate.dat to prevent SIGSEGV crash loop"
        rm -f "$GVWSTATE"
        # Fall through to auto-bootstrap logic (gvwstate.dat now missing)
    fi
fi

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

# --- AUTO-BOOTSTRAP (Incident 11 + 13 fix) ---
# When gvwstate.dat is missing (lost during NON_PRIM crash), pc.recovery cannot
# work. Two scenarios require auto-bootstrap:
#
#   A. Remote unreachable (Incident 11): cluster is partitioned, garbd is local,
#      wait then bootstrap.
#   B. Remote reachable but both NON_PRIM (Incident 13): tunnel recovered but
#      neither node has primary view history. Normal join fails repeatedly with
#      "no nodes coming from prim view, prim not possible". After MAX_JOIN_RETRIES
#      failed attempts, auto-bootstrap.
#
# Split-brain safety: only the garbd node (NL) can auto-bootstrap. GR has no
# local garbd → GARBD_REACHABLE=false → skips bootstrap → keeps retrying
# normal join until NL becomes PRIMARY.
if [ ! -f "$GVWSTATE" ] && [ -f "$GRASTATE" ]; then
    log "WARNING: gvwstate.dat missing — pc.recovery will fail"

    # Extract LOCAL_ADDR and REMOTE_ADDR from command args
    # (Incident 13 fix: WSREP_NODE_ADDRESS is not in the container env,
    # only in --wsrep-node-address command arg. Parse from args, not env.)
    REMOTE_ADDR=""
    LOCAL_ADDR=""
    CLUSTER_ADDR_RAW=""
    for arg in "$@"; do
        if [[ "$arg" == --wsrep-node-address=* ]]; then
            LOCAL_ADDR="${arg#*=}"
        fi
        if [[ "$arg" == --wsrep-cluster-address=* ]]; then
            CLUSTER_ADDR_RAW="${arg#*=}"
        fi
    done
    # Fallback to env var if not found in args
    LOCAL_ADDR="${LOCAL_ADDR:-${WSREP_NODE_ADDRESS:-}}"

    if [ -n "$CLUSTER_ADDR_RAW" ] && [ -n "$LOCAL_ADDR" ]; then
        # Parse comma-separated addresses from gcomm://addr1,addr2,addr3:port
        CLUSTER_ADDR_RAW="${CLUSTER_ADDR_RAW#gcomm://}"
        IFS=',' read -ra ADDRS <<< "$CLUSTER_ADDR_RAW"
        for addr in "${ADDRS[@]}"; do
            # Strip port suffix if present (e.g., 192.168.192.10:4570 → 192.168.192.10)
            host="${addr%%:*}"
            # Skip local address
            if [ "$host" != "$LOCAL_ADDR" ]; then
                REMOTE_ADDR="$host"
                break
            fi
        done
    fi

    log "Address detection: local=${LOCAL_ADDR:-EMPTY} remote=${REMOTE_ADDR:-EMPTY}"

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
            # --- Path A: Remote unreachable (Incident 11) ---
            log "Cluster partitioned: garbd reachable but remote data node unreachable"
            log "Waiting ${AUTO_BOOTSTRAP_DELAY}s before auto-bootstrap (tunnel may recover)..."
            sleep "$AUTO_BOOTSTRAP_DELAY"

            # Recheck remote after delay
            if timeout 3 bash -c "echo >/dev/tcp/${REMOTE_ADDR}/${REMOTE_GALERA_PORT}" 2>/dev/null; then
                log "Remote node recovered during delay — proceeding with normal join"
            else
                auto_bootstrap "remote unreachable after ${AUTO_BOOTSTRAP_DELAY}s delay"
            fi

        elif [ "$GARBD_REACHABLE" = true ] && [ "$REMOTE_REACHABLE" = true ]; then
            # --- Path B: Remote reachable but both NON_PRIM (Incident 13) ---
            # Normal join will fail with "no nodes coming from prim view" because
            # neither node has primary view history (gvwstate.dat missing/stale).
            # Track retry attempts — after MAX_JOIN_RETRIES failures, bootstrap.
            RETRIES=0
            if [ -f "$RETRY_FILE" ]; then
                RETRIES=$(cat "$RETRY_FILE" 2>/dev/null || echo 0)
            fi
            RETRIES=$((RETRIES + 1))

            if [ "$RETRIES" -ge "$MAX_JOIN_RETRIES" ]; then
                auto_bootstrap "normal join failed ${RETRIES} times — cluster has no primary (Incident 13 path)"
            else
                echo "$RETRIES" > "$RETRY_FILE"
                log "Remote reachable but no gvwstate — normal join attempt ${RETRIES}/${MAX_JOIN_RETRIES}"
                log "Will auto-bootstrap after ${MAX_JOIN_RETRIES} consecutive failures"
            fi
        fi
    fi
fi

# --- NORMAL START ---
# Clean up retry counter if gvwstate.dat exists (cluster was healthy last run)
if [ -f "$RETRY_FILE" ] && [ -f "$GVWSTATE" ]; then
    log "Cluster healthy (gvwstate.dat present) — clearing retry counter"
    rm -f "$RETRY_FILE"
fi
exec docker-entrypoint.sh "$@"
