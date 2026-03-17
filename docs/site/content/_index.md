---
title: "MeshSat Hub"
---

# MeshSat Hub

Cloud relay for [MeshSat](https://github.com/cubeos-app/meshsat) field devices. Self-hosted Docker Compose stack — no cloud dependencies beyond your Iridium gateway provider.

## What Hub Does

- **Receives** Iridium SBD messages via RockBLOCK webhook
- **Sends** MT commands to field devices via Cloudloop API
- **Relays** to TAK/CoT for situational awareness on ATAK maps
- **Injects** satellite positions into APRS-IS (visible on aprs.fi)
- **Publishes** everything to MQTT for downstream consumers
- **Rate limits** MT sends per device with SOS bypass
- **Fires** outbound webhooks on events (MO, SOS, position, telemetry)
- **Backs up** full state as ZIP with diff preview before restore

## Quick Start

```bash
git clone https://github.com/cubeos-app/meshsat-hub.git
cd meshsat-hub
cp .env.example .env   # fill in RockBLOCK secret + Cloudloop key
docker compose -f docker-compose.prod.yml up -d
```

## How It Fits

```
Field Device (Pi + radios) ──Iridium SBD──→ Ground Control webhook ──→ Hub
                                                                        │
Android (phone) ──────MQTT over Tor/WG──────────────────────────────────┤
                                                                        │
                                                    ┌───────────────────┤
                                                    ▼                   ▼
                                               TAK Server          APRS-IS
                                                    │              (aprs.fi)
                                                    ▼
                                               ATAK/WinTAK
```

Hub is **optional** — Bridge and Android work fully without it. Hub adds always-on internet services (TAK, APRS-IS, webhook forwarding, MT command delivery) that field nodes can't run themselves.

## API Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/healthz` | GET | Liveness probe |
| `/readyz` | GET | Readiness probe (MQTT connected) |
| `/api/webhook/rockblock` | POST | RockBLOCK MO webhook receiver |
| `/api/ratelimit` | GET | All device rate limit usage |
| `/api/ratelimit/{id}` | GET | Single device usage |
| `/api/ratelimit/{id}/override` | POST/DELETE | Admin rate limit exemption |
| `/api/webhooks` | GET/POST | List/create outbound webhooks |
| `/api/webhooks/{id}` | DELETE | Remove webhook |
| `/api/webhooks/logs` | GET | Webhook delivery logs |
| `/api/backup/export` | GET | Download ZIP backup |
| `/api/backup/diff` | POST | Preview restore changes |
| `/api/backup/import` | POST | Restore from ZIP |

## Links

- [GitHub](https://github.com/cubeos-app/meshsat-hub)
- [MeshSat Bridge](https://github.com/cubeos-app/meshsat)
- [MeshSat Android](https://github.com/cubeos-app/meshsat-android)
