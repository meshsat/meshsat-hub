#!/usr/bin/env bash
# Bootstrap a fresh Galera cluster from this node.
# Run on ONE node only. Other nodes join normally after.
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
ORIG_ADDR=$(grep WSREP_CLUSTER_ADDRESS .env | cut -d= -f2)

# Stop MariaDB
echo "Stopping MariaDB..."
docker compose stop mariadb 2>/dev/null || true

# Set safe_to_bootstrap
echo "Setting safe_to_bootstrap=1..."
docker run --rm -v meshsat-hub_mariadb-data:/var/lib/mysql alpine:3.21 \
  sh -c 'sed -i "s/safe_to_bootstrap:.*/safe_to_bootstrap: 1/" /var/lib/mysql/grastate.dat 2>/dev/null || true'

# Set bootstrap cluster address
echo "Setting WSREP_CLUSTER_ADDRESS=gcomm:// (bootstrap mode)..."
sed -i "s|WSREP_CLUSTER_ADDRESS=.*|WSREP_CLUSTER_ADDRESS=gcomm://|" .env

# Start MariaDB (compose reads new .env, passes gcomm:// via command args)
echo "Starting MariaDB in bootstrap mode..."
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
  # Restore original cluster address
  sed -i "s|WSREP_CLUSTER_ADDRESS=.*|WSREP_CLUSTER_ADDRESS=$ORIG_ADDR|" .env
  echo "Cluster address restored to: $ORIG_ADDR"
  echo ""
  echo "Next steps:"
  echo "  1. Start other nodes: docker compose up -d mariadb"
  echo "  2. Fresh joiner? Init DB first: see docker-compose.galera.yml header"
  echo "  3. Verify: wsrep_cluster_size=2 on both nodes"
else
  echo "ERROR: Bootstrap may have failed (size=$SIZE, ready=$READY)"
  echo "Restoring original cluster address..."
  sed -i "s|WSREP_CLUSTER_ADDRESS=.*|WSREP_CLUSTER_ADDRESS=$ORIG_ADDR|" .env
  exit 1
fi
