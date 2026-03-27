#!/usr/bin/env bash
# Bootstrap a fresh Galera cluster from this node.
# Run on ONE node only. Other nodes join normally after.
#
# Uses the flag-file bootstrap method (galera-entrypoint.sh).
# .env is NEVER modified — cluster address stays correct.
#
# Prerequisites:
#   - All other nodes STOPPED
#   - .env has MARIADB_ROOT_PASSWORD, WSREP_CLUSTER_ADDRESS, WSREP_NODE_ADDRESS, SITE_NAME
set -euo pipefail

COMPOSE_DIR="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$COMPOSE_DIR"

echo "=== Galera Bootstrap ==="
echo "This will initialize a NEW cluster from this node."
echo "All other nodes must be STOPPED before running this."
read -p "Continue? [y/N] " -n1 confirm
echo
[ "$confirm" = "y" ] || exit 0

DBPASS=$(grep MARIADB_ROOT_PASSWORD .env | cut -d= -f2)

# Verify .env has proper cluster address (refuse to run if already bare)
ENV_ADDR=$(grep WSREP_CLUSTER_ADDRESS .env | cut -d= -f2)
if echo "$ENV_ADDR" | grep -qE '^gcomm://$'; then
  echo "FATAL: .env has bare gcomm:// — fix it first:"
  echo "  WSREP_CLUSTER_ADDRESS=gcomm://192.168.192.10,192.168.15.10"
  exit 1
fi
echo "Cluster address in .env: $ENV_ADDR (will NOT be modified)"

# Stop MariaDB
echo "Stopping MariaDB..."
docker compose stop mariadb 2>/dev/null || true

# Set bootstrap flag file — galera-entrypoint.sh will:
#   1. Detect the flag
#   2. Set safe_to_bootstrap=1 in grastate.dat
#   3. Add --wsrep-new-cluster to startup args
#   4. Delete the flag (next restart joins normally)
echo "Setting bootstrap flag file..."
docker run --rm -v meshsat-hub_mariadb-data:/var/lib/mysql alpine:3.21 \
  touch /var/lib/mysql/force-bootstrap

# Start MariaDB — entrypoint handles bootstrap
echo "Starting MariaDB in bootstrap mode (via flag file)..."
docker compose up -d mariadb
echo "Waiting 30s for bootstrap..."
sleep 30

# Verify
SIZE=$(docker exec meshsat-mariadb mariadb -uroot -p"$DBPASS" -N -e \
  "SHOW STATUS LIKE 'wsrep_cluster_size'" 2>/dev/null | awk '{print $2}')
READY=$(docker exec meshsat-mariadb mariadb -uroot -p"$DBPASS" -N -e \
  "SHOW STATUS LIKE 'wsrep_ready'" 2>/dev/null | awk '{print $2}')
echo "Cluster size: $SIZE, Ready: $READY"

if [ "$SIZE" = "1" ] && [ "$READY" = "ON" ]; then
  echo "Bootstrap successful!"
  echo ""
  echo "Next steps:"
  echo "  1. Start other nodes: docker compose up -d mariadb"
  echo "  2. Start garbd:       docker compose up -d garbd"
  echo "  3. Fresh joiner? Init DB first: see docker-compose.galera.yml header"
  echo "  4. Verify: wsrep_cluster_size=3 on both nodes (2 data + 1 arbitrator)"
else
  echo "ERROR: Bootstrap may have failed (size=$SIZE, ready=$READY)"
  exit 1
fi
