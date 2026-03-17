# MeshSat Hub

Cloud-side counterpart to [MeshSat](https://github.com/cubeos-app/meshsat) field bridge devices. Self-hosted Docker Compose stack for receiving/sending Iridium SBD messages, MQTT integration, and device management.

## Quick Start

```bash
cp config.example.yaml config.yaml
# Edit config.yaml with your RockBLOCK/Cloudloop credentials
docker compose up -d
```

## Development

```bash
# Prerequisites: Go 1.24+, Docker, golangci-lint

# Run locally
make run

# Build
make build

# Test
make test

# Lint
make lint
```

## Architecture

```
Field Device (MeshSat bridge)
  → Iridium SBD → Ground Control → HTTP webhook → MeshSat Hub
  → Tor hidden service ─────────────────────────→ MeshSat Hub
  → WireGuard tunnel ──────────────────────────→ MeshSat Hub
                                                      │
                                            ┌─────────┼──────────┐
                                            ▼         ▼          ▼
                                          MQTT    Dashboard   Webhooks
```

## License

Apache 2.0 — see [LICENSE](LICENSE).
