---
title: "API Endpoints"
weight: 1
---

# API Reference

All endpoints return JSON. Authentication via `Authorization: Bearer <HUB_AUTH_TOKEN>` header.

## Health

| Method | Path | Description |
|--------|------|-------------|
| GET | `/healthz` | Liveness probe — always returns 200 |
| GET | `/readyz` | Readiness probe — 200 if MQTT connected, 503 otherwise |

## Webhooks (Inbound)

### RockBLOCK MO Webhook

```
POST /api/webhook/rockblock
Content-Type: application/x-www-form-urlencoded
```

Receives Mobile Originated SBD messages from Ground Control. Verifies HMAC-SHA256 or JWT signature, hex-decodes payload, attempts SMAZ2 decompression, publishes to MQTT.

**Form fields:** `imei`, `momsn`, `transmit_time`, `iridium_latitude`, `iridium_longitude`, `iridium_cep`, `data` (hex)

**MQTT publishes:** `meshsat/{imei}/mo/raw`, `meshsat/{imei}/mo/decoded`, `meshsat/{imei}/position` (if coordinates present)

## Rate Limiting

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/ratelimit` | Usage for all tracked devices |
| GET | `/api/ratelimit/{deviceID}` | Usage for a specific device |
| POST | `/api/ratelimit/{deviceID}/override` | Grant temporary exemption |
| DELETE | `/api/ratelimit/{deviceID}/override` | Remove exemption |

**Override request body:**
```json
{"duration_hours": 24}
```

**Usage response:**
```json
{
  "device_id": "300234063904190",
  "tokens_left": 8.5,
  "max_tokens": 10,
  "daily_sent": 3,
  "daily_cap": 100,
  "throttled": false
}
```

## Webhooks (Outbound)

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/webhooks` | List configured webhooks (secrets redacted) |
| POST | `/api/webhooks` | Create a new webhook |
| DELETE | `/api/webhooks/{id}` | Remove a webhook |
| GET | `/api/webhooks/logs` | Recent delivery logs (last 100) |

**Create webhook:**
```json
{
  "id": "flaresat",
  "url": "https://api.flaresat.io/meshsat/events",
  "secret": "hmac-signing-key",
  "events": ["mo", "sos", "position"],
  "enabled": true,
  "max_retries": 3,
  "timeout_sec": 10
}
```

**Event types:** `mo`, `sos`, `position`, `telemetry`, `mt_status`

**Webhook payload:**
```json
{
  "id": "wh-1710680400000",
  "event": "sos",
  "device_id": "300234063904190",
  "timestamp": "2026-03-17T12:00:00Z",
  "data": {"triggered": true, "lat": 52.3676, "lon": 4.9041}
}
```

Webhooks are signed with HMAC-SHA256 via the `X-Hub-Signature-256` header.

## Backup

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/backup/export` | Download ZIP backup |
| POST | `/api/backup/diff` | Upload ZIP, preview what would change |
| POST | `/api/backup/import` | Restore data files from ZIP |

**Diff response:**
```json
{
  "config_changed": true,
  "webhooks_changed": false,
  "files_added": ["new.db"],
  "files_modified": [],
  "files_removed": []
}
```
