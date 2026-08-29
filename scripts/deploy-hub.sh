#!/usr/bin/env bash
# deploy-hub.sh — Safe Hub deployment that NEVER touches MariaDB/Galera.
#
# Usage: ./scripts/deploy-hub.sh
#
# NEVER run: docker compose up -d        (restarts MariaDB, breaks Galera!)
# ALWAYS run: docker compose up -d --no-deps hub
set -euo pipefail

COMPOSE_DIR="/srv/meshsat-hub"
TIMEOUT=60

log() { echo "[$(date +%H:%M:%S)] $*"; }
die() { log "FATAL: $*"; exit 1; }

log "=== MeshSat Hub Safe Deploy ==="

# Step 1: Pre-deploy Galera health check
log "Step 1: Checking Galera cluster health..."
DBPASS=$(grep MARIADB_ROOT_PASSWORD "$COMPOSE_DIR/.env" | cut -d= -f2)
GALERA_STATUS=$(docker exec meshsat-mariadb mariadb -uroot -p"$DBPASS" -N -e \
  "SELECT VARIABLE_VALUE FROM information_schema.global_status WHERE VARIABLE_NAME='wsrep_local_state_comment';" 2>/dev/null || echo "UNREACHABLE")

if [ "$GALERA_STATUS" != "Synced" ]; then
  die "Galera NOT synced (status: $GALERA_STATUS). Run galera-watchdog.sh first."
fi
log "Galera: Synced ✓"

# Step 2: Pull new image
log "Step 2: Pulling latest Hub image..."
docker pull ghcr.io/meshsat/meshsat-hub:latest || die "Pull failed"

# Step 3: Restart ONLY Hub (--no-deps = never touch MariaDB/Redis/NATS)
log "Step 3: Restarting Hub container (--no-deps)..."
cd "$COMPOSE_DIR"
docker compose up -d --no-deps hub || die "Hub restart failed"

# Step 4: Wait for healthcheck
log "Step 4: Waiting for Hub healthcheck..."
for i in $(seq 1 $TIMEOUT); do
  STATUS=$(docker inspect --format='{{.State.Health.Status}}' meshsat-hub 2>/dev/null || echo "unknown")
  [ "$STATUS" = "healthy" ] && { log "Hub healthy after ${i}s ✓"; break; }
  [ "$i" = "$TIMEOUT" ] && die "Hub not healthy after ${TIMEOUT}s"
  sleep 1
done

# Step 5: Verify endpoints
HEALTHZ=$(curl -sf http://localhost:6070/healthz 2>/dev/null || echo "FAIL")
echo "$HEALTHZ" | grep -q '"ok"' && log "/healthz: OK ✓" || die "/healthz FAILED"

# Step 6: Verify Galera untouched
GALERA_POST=$(docker exec meshsat-mariadb mariadb -uroot -p"$DBPASS" -N -e \
  "SELECT VARIABLE_VALUE FROM information_schema.global_status WHERE VARIABLE_NAME='wsrep_local_state_comment';" 2>/dev/null || echo "UNREACHABLE")
[ "$GALERA_POST" = "Synced" ] && log "Galera still synced ✓" || log "WARNING: Galera $GALERA_POST"

log "=== Deploy complete ==="
