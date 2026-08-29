---
title: "Installation"
weight: 1
---

# Installation

## Prerequisites

- Docker Engine 24+ and Docker Compose v2
- Public IP with domain name (for TLS)
- Iridium RockBLOCK account (Ground Control portal access)
- Cloudloop API key (for MT sends)

## Docker Compose (Production)

```bash
git clone https://github.com/meshsat/meshsat-hub.git
cd meshsat-hub

# Configure
cp .env.example .env
nano .env    # set HUB_ROCKBLOCK_SECRET, HUB_CLOUDLOOP_API_KEY, HUB_DEVICE_IMEI
nano Caddyfile  # replace hub.meshsat.io with your domain

# Start
docker compose -f docker-compose.prod.yml up -d
```

## Services

| Service | Port | Purpose |
|---------|------|---------|
| Hub | 6070 (internal) | Go REST API |
| Caddy | 80, 443 | Reverse proxy + auto TLS |
| Mosquitto | 6071, 6072 | MQTT broker |
| Tor | — | .onion hidden service |
| OpenTAKServer | 8087, 8089, 8443 | TAK/CoT (optional, `--profile tak`) |
| Prometheus | 9090 (internal) | Monitoring (optional, `--profile monitoring`) |

## Ground Control Webhook

1. Log in to [Ground Control](https://rockblock.rock7.com)
2. Device → Delivery Groups → Add HTTP endpoint
3. URL: `https://your-domain/api/webhook/rockblock`
4. Secret: same as `HUB_ROCKBLOCK_SECRET` in `.env`

## Build from Source

```bash
make build              # → bin/meshsat-hub
make test               # unit tests
make test-integration   # integration tests (embedded MQTT)
make lint               # golangci-lint
```
