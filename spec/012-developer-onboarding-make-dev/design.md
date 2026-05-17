# Design — Developer Onboarding `make dev`

## Goal

Zero-friction contributor onboarding per `docs/ROADMAP.md` L151. The repo README's "Quickstart" today expects an Iridium modem + Mosquitto + manual env setup. `make dev` collapses all of it into one command.

## docker-compose.dev.yml

```yaml
services:
  meshsat-hub:
    image: ghcr.io/cubeos-app/meshsat-hub:latest  # or build from source
    environment:
      HUB_MODE: standalone
      HUB_SIMULATOR_ENABLED: "true"
      HUB_LOG_LEVEL: debug
      HUB_AUTH_TOKEN: dev-token-do-not-use-in-prod
      HUB_DATABASE_URL: sqlite:///data/hub.db
      HUB_MQTT_BROKER_URL: tcp://mosquitto:1883
    ports:
      - "6070:6070"
    volumes:
      - meshsat-dev-data:/data
    depends_on:
      - mosquitto

  mosquitto:
    image: eclipse-mosquitto:2
    ports:
      - "1883:1883"

  tak-server:
    profiles: ["tak"]   # only on `make dev-tak`
    image: opentakserver/opentakserver:latest

volumes:
  meshsat-dev-data:
```

## Makefile targets

```makefile
.PHONY: dev dev-down dev-clean dev-logs dev-shell

dev:
	@docker compose version > /dev/null 2>&1 || (echo "ERROR: docker compose required"; exit 1)
	@docker compose -f docker-compose.dev.yml up -d
	@./scripts/dev-wait-for-hub.sh   # polls localhost:6070/healthz
	@./scripts/dev-seed-simulators.sh  # POSTs 3 simulator starts
	@echo "Hub up at http://localhost:6070 — try the Dashboard"

dev-down:
	@docker compose -f docker-compose.dev.yml down

dev-clean:
	@docker compose -f docker-compose.dev.yml down -v
	@docker volume rm meshsat-dev-data 2>/dev/null || true

dev-logs:
	@docker compose -f docker-compose.dev.yml logs -f

dev-shell:
	@docker compose -f docker-compose.dev.yml exec meshsat-hub /bin/sh
```

## scripts/dev-seed-simulators.sh

Hits the simulator API from spec/010:

```bash
curl -X POST http://localhost:6070/api/simulator/start \
  -H "Authorization: Bearer dev-token-do-not-use-in-prod" \
  -d '{"device_imei":"000000000000001","message_rate_per_minute":6,"payload_pattern":"text"}'
# ... repeat for 2 + 3 with patterns position, sos
```

## Why simulated SOS? (REQ-1103)

The third simulator generates SOS messages. New contributors see the SOS detector (spec/001) fire end-to-end on their first `make dev`. This makes the audit log + escalation chain visible immediately rather than buried behind "configure a real device first".

## Out of scope

- No multi-arch Docker base image — uses standard amd64.
- No Kubernetes onboarding (`HUB_MODE=kubernetes`) — that's production deployment, separate ADR / docs.
- No live-reload of Go binary on contributor edits — contributor still rebuilds with `make build && make dev-down && make dev`.
