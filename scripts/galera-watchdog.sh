#!/usr/bin/env bash
# galera-watchdog.sh — Monitors Galera cluster and auto-bootstraps on quorum loss.
#
# Usage:
#   ./scripts/galera-watchdog.sh          # run once (check + fix if needed)
#   ./scripts/galera-watchdog.sh --loop   # run continuously every 30s
#
# What it does:
#   1. Checks wsrep_local_state_comment
#   2. If "Synced" → all good
#   3. If not synced → stop MariaDB, set safe_to_bootstrap=1, restart with gcomm://
#   4. After bootstrap, restore cluster address and start Hub
set -euo pipefail

COMPOSE_DIR="/srv/meshsat-hub"
LOOP="${1:-}"
INTERVAL=30

log() { echo "[$(date +%H:%M:%S)] galera-watchdog: $*"; }

check_and_fix() {
  cd "$COMPOSE_DIR"
  DBPASS=$(grep MARIADB_ROOT_PASSWORD .env | cut -d= -f2)

  # Check if MariaDB container is running
  if ! docker ps --filter name=meshsat-mariadb --format '{{.Names}}' | grep -q meshsat-mariadb; then
    log "MariaDB container not running — starting..."
    docker compose start mariadb 2>/dev/null || true
    sleep 20
  fi

  # Check Galera status
  GALERA_STATUS=$(docker exec meshsat-mariadb mariadb -uroot -p"$DBPASS" -N -e \
    "SELECT VARIABLE_VALUE FROM information_schema.global_status WHERE VARIABLE_NAME='wsrep_local_state_comment';" 2>/dev/null || echo "UNREACHABLE")

  if [ "$GALERA_STATUS" = "Synced" ]; then
    # Also check Hub is running
    HUB_STATUS=$(docker inspect --format='{{.State.Health.Status}}' meshsat-hub 2>/dev/null || echo "unknown")
    if [ "$HUB_STATUS" = "healthy" ]; then
      return 0  # all good
    else
      log "Galera OK but Hub unhealthy ($HUB_STATUS) — restarting Hub..."
      docker compose up -d --no-deps hub 2>/dev/null
      sleep 10
      return 0
    fi
  fi

  log "GALERA QUORUM LOST (status: $GALERA_STATUS) — initiating bootstrap..."

  # Stop Hub and MariaDB
  log "Stopping Hub + MariaDB..."
  docker compose stop hub mariadb 2>/dev/null || true
  sleep 3

  # Set safe_to_bootstrap in grastate
  log "Setting safe_to_bootstrap=1..."
  docker run --rm -v meshsat-hub_mariadb-data:/var/lib/mysql alpine:3.21 \
    sh -c 'sed -i "s/safe_to_bootstrap:.*/safe_to_bootstrap: 1/" /var/lib/mysql/grastate.dat' 2>/dev/null

  # Temporarily set bootstrap address
  ORIG_ADDR=$(grep WSREP_CLUSTER_ADDRESS .env | cut -d= -f2)
  log "Original cluster address: $ORIG_ADDR"
  sed -i "s|WSREP_CLUSTER_ADDRESS=.*|WSREP_CLUSTER_ADDRESS=gcomm://|" .env

  # Start MariaDB with bootstrap address
  log "Starting MariaDB with gcomm:// (bootstrap)..."
  docker compose up -d mariadb
  sleep 30

  # Verify bootstrap
  GALERA_STATUS=$(docker exec meshsat-mariadb mariadb -uroot -p"$DBPASS" -N -e \
    "SELECT VARIABLE_VALUE FROM information_schema.global_status WHERE VARIABLE_NAME='wsrep_local_state_comment';" 2>/dev/null || echo "UNREACHABLE")

  if [ "$GALERA_STATUS" != "Synced" ]; then
    log "BOOTSTRAP FAILED (status: $GALERA_STATUS) — manual intervention needed"
    # Restore original address
    sed -i "s|WSREP_CLUSTER_ADDRESS=.*|WSREP_CLUSTER_ADDRESS=$ORIG_ADDR|" .env
    return 1
  fi

  log "Galera bootstrapped: Synced ✓"

  # Restore original cluster address (don't restart MariaDB — it's running fine)
  sed -i "s|WSREP_CLUSTER_ADDRESS=.*|WSREP_CLUSTER_ADDRESS=$ORIG_ADDR|" .env
  log "Cluster address restored (MariaDB still running with bootstrap, will rejoin on next restart)"

  # Start Hub
  log "Starting Hub..."
  docker compose up -d --no-deps hub
  sleep 15

  # Verify Hub
  HUB_STATUS=$(docker inspect --format='{{.State.Health.Status}}' meshsat-hub 2>/dev/null || echo "unknown")
  if [ "$HUB_STATUS" = "healthy" ]; then
    log "Hub healthy ✓ — RECOVERY COMPLETE"
  else
    log "Hub status: $HUB_STATUS (may still be starting)"
  fi
}

if [ "$LOOP" = "--loop" ]; then
  log "Starting continuous monitoring (every ${INTERVAL}s)..."
  while true; do
    check_and_fix || log "Check failed — will retry"
    sleep $INTERVAL
  done
else
  check_and_fix
fi
