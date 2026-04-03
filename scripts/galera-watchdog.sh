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
#   3. If not synced → stop MariaDB, set bootstrap flag file, restart
#   4. After bootstrap, start garbd + Hub
#
# IMPORTANT: This script NEVER modifies .env. Bootstrap uses the flag-file
# method via galera-entrypoint.sh. See CLAUDE.md Galera rules.
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
    # Use --force-recreate to ensure command args reflect current .env values
    docker compose up -d --no-deps --force-recreate mariadb 2>/dev/null || true
    sleep 20
  fi

  # Check Galera status
  GALERA_STATUS=$(docker exec meshsat-mariadb mariadb -uroot -p"$DBPASS" -N -e \
    "SELECT VARIABLE_VALUE FROM information_schema.global_status WHERE VARIABLE_NAME='wsrep_local_state_comment';" 2>/dev/null || echo "UNREACHABLE")

  if [ "$GALERA_STATUS" = "Synced" ]; then
    # Check garbd is running (3rd quorum voter)
    if ! docker ps --filter name=meshsat-garbd --format '{{.Names}}' | grep -q meshsat-garbd; then
      log "garbd not running — restarting..."
      docker restart meshsat-garbd 2>/dev/null || docker compose up -d garbd 2>/dev/null || true
      sleep 5
    fi

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

  # Verify .env has proper cluster address (safety check)
  ENV_ADDR=$(grep WSREP_CLUSTER_ADDRESS .env | cut -d= -f2)
  if echo "$ENV_ADDR" | grep -qE '^gcomm://$'; then
    log "FATAL: .env already has bare gcomm:// — manual intervention needed"
    log "Fix .env first: WSREP_CLUSTER_ADDRESS=gcomm://192.168.192.10,192.168.15.10"
    return 1
  fi

  # Stop Hub and MariaDB
  log "Stopping Hub + MariaDB..."
  docker compose stop hub mariadb 2>/dev/null || true
  sleep 3

  # Set bootstrap flag file (galera-entrypoint.sh handles the rest)
  # This also sets safe_to_bootstrap=1 in grastate.dat
  log "Setting bootstrap flag file..."
  docker run --rm -v meshsat-hub_mariadb-data:/var/lib/mysql alpine:3.21 \
    touch /var/lib/mysql/force-bootstrap

  # Start MariaDB — entrypoint detects flag, adds --wsrep-new-cluster, deletes flag
  # .env is NEVER modified — cluster address stays correct for next normal restart
  # IMPORTANT: --force-recreate is required because Docker Compose v5 does not
  # update baked-in command args without it, even when .env has changed.
  log "Starting MariaDB with bootstrap flag..."
  docker compose up -d --no-deps --force-recreate mariadb
  sleep 30

  # Verify bootstrap
  GALERA_STATUS=$(docker exec meshsat-mariadb mariadb -uroot -p"$DBPASS" -N -e \
    "SELECT VARIABLE_VALUE FROM information_schema.global_status WHERE VARIABLE_NAME='wsrep_local_state_comment';" 2>/dev/null || echo "UNREACHABLE")

  if [ "$GALERA_STATUS" != "Synced" ]; then
    log "BOOTSTRAP FAILED (status: $GALERA_STATUS) — manual intervention needed"
    return 1
  fi

  log "Galera bootstrapped: Synced ✓"

  # Restart garbd (quorum voter)
  log "Restarting garbd..."
  docker restart meshsat-garbd 2>/dev/null || docker compose up -d garbd 2>/dev/null || true
  sleep 5

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
