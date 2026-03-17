# MeshSat Hub

Cloud-side counterpart to [MeshSat](https://github.com/cubeos-app/meshsat) field bridge devices. Receives Iridium SBD messages via RockBLOCK/Ground Control webhook, sends outbound messages via Cloudloop API, and publishes everything to MQTT for downstream consumers.

Self-hosted Docker Compose stack — no cloud dependencies beyond your Iridium gateway provider.

## Features

- **MO ingestion** — RockBLOCK webhook receives Mobile Originated SBD messages, hex-decodes, decompresses (SMAZ2), and publishes to MQTT
- **MT sending** — Subscribe to MQTT `mt/send` topic, Hub forwards to Cloudloop REST API with retry and status tracking
- **SMAZ2 compression** — Meshtastic-compatible dictionary for efficient satellite text encoding (typically 40-60% compression)
- **MQTT namespace** — Structured topic hierarchy: `meshsat/{device_id}/mo/decoded`, `mt/status`, `position`, etc.
- **Tor hidden service** — Field devices with internet access can connect directly via `.onion` address
- **Health probes** — Kubernetes-compatible `/healthz` (liveness) and `/readyz` (readiness) endpoints

## Architecture

```
Field Device (Iridium 9603N)
    │
    ▼
Iridium Constellation ──► Ground Control ──► POST /api/webhook/rockblock
                                                      │
                               ┌──────────────────────┤
                               ▼                      ▼
                          mo/raw (MQTT)         mo/decoded (MQTT)
                                                      │
                          ┌───────────────────────────┼──────────┐
                          ▼                           ▼          ▼
                      Dashboard                  Notifications   TAK
                          │
                          ▼
                    mt/send (MQTT) ──► Hub ──► Cloudloop API ──► Iridium ──► Device
```

### Stack Components

| Service | Image | Port | Purpose |
|---------|-------|------|---------|
| Hub | Custom Go binary | 6070 | API server, webhook handler, MT sender |
| Mosquitto | eclipse-mosquitto:2 | 6071 (TCP), 6072 (WS) | MQTT broker |
| Tor | cmehay/docker-tor-hidden-service | .onion:80, .onion:1883 | Tor v3 hidden service |

## Quick Start

```bash
# 1. Configure
cp config.example.yaml config.yaml
# Edit config.yaml — set rockblock_secret and cloudloop_api_key

# 2. Start
docker compose up -d

# 3. Verify
curl http://localhost:6070/healthz
# → {"status":"ok"}
```

### Configuration

All settings can be set via `config.yaml` or environment variables (prefix `HUB_`):

| Setting | Env Var | Default | Description |
|---------|---------|---------|-------------|
| `port` | `HUB_PORT` | 6070 | HTTP API port |
| `mqtt_broker_url` | `HUB_MQTT_BROKER_URL` | `tcp://mqtt:1883` | MQTT broker address |
| `mqtt_client_id` | `HUB_MQTT_CLIENT_ID` | `meshsat-hub` | MQTT client identifier |
| `rockblock_secret` | `HUB_ROCKBLOCK_SECRET` | — | Shared secret for webhook verification |
| `cloudloop_api_key` | `HUB_CLOUDLOOP_API_KEY` | — | Cloudloop REST API key |
| `cloudloop_api_url` | `HUB_CLOUDLOOP_API_URL` | `https://api.cloudloop.com` | Cloudloop API base URL |
| `device_imei` | `HUB_DEVICE_IMEI` | — | Default device IMEI (single-device mode) |
| `log_level` | `HUB_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `log_format` | `HUB_LOG_FORMAT` | `json` | `json` or `text` |

## MQTT Topics

All messages follow the namespace `meshsat/{device_id}/...`:

| Topic | Dir | QoS | Description |
|-------|-----|-----|-------------|
| `meshsat/{id}/mo/raw` | Hub → | 1 | Raw MO payload (base64-encoded) |
| `meshsat/{id}/mo/decoded` | Hub → | 1 | Decoded MO message with metadata |
| `meshsat/{id}/mt/send` | → Hub | 1 | Queue outbound MT message |
| `meshsat/{id}/mt/status` | Hub → | 1 | MT delivery status updates |
| `meshsat/{id}/position` | Hub → | 1 | Iridium CEP position (retained) |
| `meshsat/hub/status` | Hub → | 0 | Hub health (retained) |

### Example: mo/decoded payload

```json
{
  "imei": "300234065123456",
  "momsn": 42,
  "channel": "iridium",
  "text": "All clear, moving to checkpoint B",
  "compressed": true,
  "compression": "smaz2",
  "transmit_time": "26-03-17 10:30:00",
  "iridium_latitude": 52.1621,
  "iridium_longitude": 4.5094,
  "iridium_cep": 10
}
```

### Example: Send an MT message

```bash
mosquitto_pub -h localhost -p 6071 \
  -t "meshsat/300234065123456/mt/send" \
  -m '{"text":"Acknowledged, proceed to checkpoint B","compress":true}'
```

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/webhook/rockblock` | RockBLOCK/Ground Control MO webhook |
| `GET` | `/healthz` | Liveness probe (always 200) |
| `GET` | `/readyz` | Readiness probe (200 if MQTT connected) |

## Development

```bash
# Prerequisites: Go 1.24+, Docker Compose v2

# Run locally (starts Go service with debug logging)
make run

# Build binary
make build          # → bin/meshsat-hub
make build-arm64    # → bin/meshsat-hub-arm64

# Tests
make test                # Unit tests (22 tests, no external deps)
make test-integration    # Integration tests (13 tests, embedded MQTT broker)

# Code quality
make fmt
make lint
```

### Project Structure

```
meshsat-hub/
├── cmd/meshsat-hub/        # Entrypoint, router setup
├── internal/
│   ├── cloudloop/          # Cloudloop REST API client + MT sender
│   ├── compress/           # SMAZ2 compression (Meshtastic dictionary)
│   ├── config/             # YAML + env config loading
│   ├── health/             # Liveness/readiness probes
│   ├── mqtt/               # Paho MQTT wrapper + topic helpers
│   └── rockblock/          # RockBLOCK webhook handler
├── test/integration/       # End-to-end tests (embedded MQTT broker)
├── mosquitto/config/       # Mosquitto broker configuration
├── docker-compose.yml      # Full stack (hub + mosquitto + tor)
├── Dockerfile              # Multi-stage Alpine build
└── config.example.yaml     # Example configuration
```

## Related Projects

| Project | Description |
|---------|-------------|
| [meshsat](https://github.com/cubeos-app/meshsat) | Field bridge firmware (Go, Raspberry Pi/SBC) |
| [meshsat-android](https://github.com/cubeos-app/meshsat-android) | Android gateway app (Kotlin) |

## License

Apache 2.0 — see [LICENSE](LICENSE).
