---
title: "Configuration"
weight: 2
---

# Configuration

Hub reads config from YAML (`config.yaml`) with environment variable overrides (prefix `HUB_`).

## Environment Variables

### Required

| Variable | Description |
|----------|-------------|
| `HUB_ROCKBLOCK_SECRET` | Shared secret for RockBLOCK webhook HMAC verification |
| `HUB_CLOUDLOOP_API_KEY` | Cloudloop REST API key for MT message sends |
| `HUB_DEVICE_IMEI` | Default device IMEI (single-device mode) |

### Optional

| Variable | Default | Description |
|----------|---------|-------------|
| `HUB_PORT` | `6070` | HTTP API port |
| `HUB_MQTT_BROKER_URL` | `tcp://mqtt:1883` | MQTT broker URL |
| `HUB_MQTT_CLIENT_ID` | `meshsat-hub` | MQTT client identifier |
| `HUB_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `HUB_LOG_FORMAT` | `json` | `json` or `text` |
| `HUB_AUTH_TOKEN` | — | Bearer token for API access |

### TAK/CoT

| Variable | Default | Description |
|----------|---------|-------------|
| `HUB_TAK_ENABLED` | `false` | Enable TAK gateway |
| `HUB_TAK_HOST` | `opentakserver` | TAK server hostname |
| `HUB_TAK_PORT` | `8087` | TCP CoT port |
| `HUB_TAK_SSL` | `false` | Use TLS for TAK connection |
| `HUB_TAK_CALLSIGN_PREFIX` | `MESHSAT-HUB` | CoT callsign prefix |
| `HUB_TAK_COT_STALE_SECONDS` | `600` | CoT event stale time |

### APRS-IS

| Variable | Default | Description |
|----------|---------|-------------|
| `HUB_APRSIS_ENABLED` | `false` | Enable APRS-IS IGate |
| `HUB_APRSIS_SERVER` | `euro.aprs2.net:14580` | APRS-IS server |
| `HUB_APRSIS_CALLSIGN` | — | Amateur radio callsign |
| `HUB_APRSIS_PASSCODE` | — | APRS-IS verification passcode |

### Rate Limiting

| Variable | Default | Description |
|----------|---------|-------------|
| `HUB_RATELIMIT_BURST` | `10` | Max burst tokens per device |
| `HUB_RATELIMIT_REFILL_PER_MIN` | `1` | Tokens refilled per minute |
| `HUB_RATELIMIT_DAILY_CAP` | `100` | Max sends per device per day (0 = unlimited) |

## MQTT Topic Namespace

```
meshsat/{device_id}/mo/raw          # Raw MO payload (base64, QoS 1)
meshsat/{device_id}/mo/decoded      # Decoded message (JSON, QoS 1)
meshsat/{device_id}/mt/send         # Queue MT message (JSON, QoS 1)
meshsat/{device_id}/mt/status       # MT delivery status (JSON, QoS 1)
meshsat/{device_id}/position        # GPS position (JSON, QoS 1, retained)
meshsat/{device_id}/telemetry       # Sensor data (JSON, QoS 0, retained)
meshsat/{device_id}/sos             # SOS event (JSON, QoS 2)
meshsat/{device_id}/config/current  # Config snapshot (JSON, QoS 1, retained)
meshsat/hub/status                  # Hub health (JSON, QoS 0, retained)
meshsat/hub/events                  # System events (JSON, QoS 1)
meshsat/hub/credits                 # Iridium credit balance (JSON, QoS 0, retained)
```
