#!/usr/bin/env bash
# deploy-hub.sh — Safe rolling update of the Hub container ONLY.
# NEVER touches MariaDB, Redis, NATS, nginx, or garbd.
#
# Usage: ./scripts/deploy-hub.sh [image_tag]
# Default tag: latest
#
# This script is safe to run on a live Galera cluster.

set -euo pipefail

COMPOSE_DIR="${COMPOSE_DIR:-/srv/meshsat-hub}"
IMAGE="${MESHSAT_HUB_IMAGE:-ghcr.io/cubeos-app/meshsat-hub}"
TAG="${1:-latest}"

cd "$COMPOSE_DIR"

echo "[deploy] Pulling ${IMAGE}:${TAG}..."
docker pull "${IMAGE}:${TAG}"

echo "[deploy] Recreating Hub container only (--no-deps --force-recreate)..."
docker compose up -d --no-deps --force-recreate hub

echo "[deploy] Waiting for Hub to become healthy..."
for i in $(seq 1 30); do
    sleep 2
    if docker exec meshsat-hub wget -qO- http://localhost:6070/healthz 2>/dev/null | grep -q ok; then
        echo "[deploy] Hub healthy after ${i}x2s"
        exit 0
    fi
done

echo "[deploy] WARNING: Hub did not become healthy in 60s"
docker logs meshsat-hub --tail 10
exit 1
