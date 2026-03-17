---
title: "Monitoring"
weight: 2
---

# Monitoring

## Health Endpoints

| Endpoint | Response | Use |
|----------|----------|-----|
| `GET /healthz` | 200 always | Container liveness |
| `GET /readyz` | 200 if MQTT connected, 503 otherwise | Readiness gate |

## Docker Health Checks

```bash
docker compose -f docker-compose.prod.yml ps
watch -n 10 'docker compose -f docker-compose.prod.yml ps --format "table {{.Name}}\t{{.Status}}"'
```

## Logs

```bash
# All services
docker compose -f docker-compose.prod.yml logs -f

# Hub only (JSON structured)
docker compose -f docker-compose.prod.yml logs -f hub

# Caddy access
docker exec meshsat-caddy cat /data/access.log
```

## Prometheus (Optional)

Enable with `--profile monitoring`:

```bash
docker compose -f docker-compose.prod.yml --profile monitoring up -d
```

Prometheus at `http://localhost:9090` (internal only). Basic up/down scrape for Hub and MQTT.

## Key Metrics to Watch

- **Hub healthz** — if 503, MQTT is disconnected
- **MQTT clients connected** — `mosquitto_sub -t '$SYS/broker/clients/connected' -C 1`
- **Rate limit alerts** — subscribe to `meshsat/hub/events` for throttle/cap events
- **Webhook delivery logs** — `GET /api/webhooks/logs` for failed deliveries
