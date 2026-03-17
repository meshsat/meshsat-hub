# MeshSat Hub — Production Deployment Guide

## Prerequisites

- VPS with public IP (1 CPU, 512MB RAM minimum, 1GB recommended)
- Ubuntu 22.04+ or Debian 12+
- Docker Engine 24+ and Docker Compose v2
- Domain name pointed at the VPS IP (for TLS)

## Quick Start

```bash
# 1. Clone
git clone https://github.com/cubeos-app/meshsat-hub.git
cd meshsat-hub

# 2. Configure
cp .env.example .env
nano .env  # fill in secrets

# 3. Edit Caddyfile — replace hub.meshsat.io with your domain
nano Caddyfile

# 4. Start
docker compose -f docker-compose.prod.yml up -d

# 5. Verify
curl https://your-domain.com/healthz
```

## Architecture

```
Internet
    │
    ▼
Caddy (port 80/443, automatic TLS via Let's Encrypt)
    │
    ├─ /api/*     → Hub (port 6070, internal only)
    ├─ /healthz   → Hub
    └─ /readyz    → Hub

Field devices connect directly to:
    ├─ MQTT TCP   → Mosquitto (port 6071)
    ├─ MQTT WS    → Mosquitto (port 6072)
    └─ Tor        → .onion:80 (Hub), .onion:1883 (MQTT)
```

## Configuration

### Required Environment Variables

| Variable | Description |
|----------|-------------|
| `HUB_ROCKBLOCK_SECRET` | Shared secret for RockBLOCK webhook verification |
| `HUB_CLOUDLOOP_API_KEY` | Cloudloop REST API key for MT sends |
| `HUB_DEVICE_IMEI` | Default device IMEI (single-device mode) |
| `HUB_AUTH_TOKEN` | Auth token for API access |

### Optional: TAK/CoT

Enable TAK integration with OpenTAKServer:

```bash
# In .env
HUB_TAK_ENABLED=true

# Start with TAK profile
docker compose -f docker-compose.prod.yml --profile tak up -d
```

TAK clients (ATAK/WinTAK/iTAK) connect to:
- TCP CoT: `<your-ip>:8087`
- TLS CoT: `<your-ip>:8089`
- DataPackage: `<your-ip>:8443`

### Optional: APRS-IS

Enable APRS-IS IGate for satellite position injection:

```bash
# In .env (requires amateur radio license)
HUB_APRSIS_ENABLED=true
HUB_APRSIS_CALLSIGN=PA3XYZ
HUB_APRSIS_PASSCODE=12345
```

### Optional: Monitoring

Enable Prometheus metrics collection:

```bash
docker compose -f docker-compose.prod.yml --profile monitoring up -d
```

Prometheus available at `http://localhost:9090` (internal only).

## TLS Configuration

Caddy handles TLS automatically via Let's Encrypt. Requirements:
- Port 80 and 443 open to the internet
- Domain A/AAAA record pointing to the VPS
- Edit `Caddyfile` to replace `hub.meshsat.io` with your domain

For custom certificates (e.g., behind HAProxy):
```
hub.meshsat.io {
    tls /path/to/cert.pem /path/to/key.pem
    reverse_proxy hub:6070
}
```

## Resource Sizing

| Deployment | CPU | RAM | Disk | Devices |
|-----------|-----|-----|------|---------|
| Minimal | 1 vCPU | 512MB | 5GB | 1-5 |
| Standard | 1 vCPU | 1GB | 10GB | 5-50 |
| With TAK | 2 vCPU | 2GB | 20GB | 50+ |

Hub itself uses ~30MB RAM. Mosquitto adds ~20MB. Caddy adds ~30MB. OpenTAKServer adds ~500MB.

## Ground Control Webhook Setup

1. Log in to [Ground Control portal](https://rockblock.rock7.com)
2. Navigate to your RockBLOCK device → Delivery Groups
3. Add HTTP delivery endpoint:
   - URL: `https://your-domain.com/api/webhook/rockblock`
   - Shared secret: same value as `HUB_ROCKBLOCK_SECRET`
4. Test with "Send Test Message"

## Backup and Restore

### Export
```bash
curl -o backup.zip https://your-domain.com/api/backup/export \
  -H "Authorization: Bearer $HUB_AUTH_TOKEN"
```

### Preview changes before import
```bash
curl -X POST https://your-domain.com/api/backup/diff \
  -H "Content-Type: application/zip" \
  --data-binary @backup.zip
```

### Import
```bash
curl -X POST https://your-domain.com/api/backup/import \
  -H "Authorization: Bearer $HUB_AUTH_TOKEN" \
  -H "Content-Type: application/zip" \
  --data-binary @backup.zip
```

## Tor Hidden Service

The Tor container automatically creates a v3 .onion address on first start. To find it:

```bash
docker exec meshsat-tor cat /var/lib/tor/hidden_service/hostname
```

Field devices can connect via:
- Hub API: `http://<onion-address>:80`
- MQTT: `<onion-address>:1883` (via SOCKS5 proxy)

**Important:** Back up the Tor keys volume — losing it means losing the .onion address:
```bash
docker run --rm -v meshsat-hub_tor-keys:/data -v $(pwd):/backup alpine \
  tar czf /backup/tor-keys-backup.tar.gz /data
```

## Troubleshooting

| Issue | Fix |
|-------|-----|
| `healthz` returns 503 | MQTT not connected — check `docker logs meshsat-mqtt` |
| Webhook returns 401 | Wrong `HUB_ROCKBLOCK_SECRET` — verify against Ground Control |
| TAK not forwarding | Check `HUB_TAK_ENABLED=true` and `docker logs meshsat-hub` for TAK errors |
| Caddy won't start | Port 80/443 in use — stop other web servers, or configure Caddy for different ports |
| MQTT unreachable | Firewall blocking port 6071 — `ufw allow 6071/tcp` |

## Updating

```bash
docker compose -f docker-compose.prod.yml pull
docker compose -f docker-compose.prod.yml up -d
```

Hub image is rebuilt on every push to main via GitLab CI.

## Logs

```bash
# Hub logs
docker logs -f meshsat-hub

# MQTT broker logs
docker logs -f meshsat-mqtt

# Caddy access logs
docker exec meshsat-caddy cat /data/access.log
```
