#!/usr/bin/env bash
# garbd-entrypoint.sh — Startup wrapper for Galera Arbitrator (garbd).
#
# Prevents garbd from forming a solo PRIMARY component with proto 127/127,
# which poisons the cluster and causes MariaDB nodes to crash-loop (SIGSEGV).
#
# Root cause (Incident 14, 2026-04-09): Docker's restart: unless-stopped
# ignores depends_on ordering during crash recovery. garbd can restart before
# MariaDB, self-promote to PRIMARY via pc.recovery, and advertise protocol
# versions 127/127 (garbd's internal max). MariaDB 11.3.2 only supports
# repl_proto: 10, appl_proto: 4, so it aborts on join.
#
# Fix: wait for at least one MariaDB data node to be reachable before
# starting garbd. This ensures garbd always JOINS an existing primary
# rather than creating one.

set -euo pipefail

# Data node addresses (both sites)
DATA_NODE_NL="192.168.192.10"
DATA_NODE_GR="192.168.15.10"
GALERA_PORT=4567
MAX_WAIT=300       # 5 minutes max wait
CHECK_INTERVAL=5   # seconds between checks

log() {
    echo "garbd-entrypoint: $(date -u '+%Y-%m-%d %H:%M:%S') $*"
}

log "Waiting for at least one MariaDB data node before starting garbd..."
log "This prevents garbd from forming a solo PRIMARY with proto 127/127"

elapsed=0
while [ "$elapsed" -lt "$MAX_WAIT" ]; do
    # Check NL data node
    if timeout 3 bash -c "echo >/dev/tcp/${DATA_NODE_NL}/${GALERA_PORT}" 2>/dev/null; then
        log "Data node NL (${DATA_NODE_NL}:${GALERA_PORT}) is reachable — starting garbd"
        exec garbd "$@"
    fi

    # Check GR data node
    if timeout 3 bash -c "echo >/dev/tcp/${DATA_NODE_GR}/${GALERA_PORT}" 2>/dev/null; then
        log "Data node GR (${DATA_NODE_GR}:${GALERA_PORT}) is reachable — starting garbd"
        exec garbd "$@"
    fi

    log "No data node reachable (waited ${elapsed}s/${MAX_WAIT}s) — retrying in ${CHECK_INTERVAL}s..."
    sleep "$CHECK_INTERVAL"
    elapsed=$((elapsed + CHECK_INTERVAL))
done

log "FATAL: No data node reachable after ${MAX_WAIT}s — refusing to start garbd"
log "Manual intervention required: start MariaDB on at least one node first"
exit 1
