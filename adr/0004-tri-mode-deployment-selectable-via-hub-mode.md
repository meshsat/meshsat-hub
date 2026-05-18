# 4. Tri-mode deployment via HUB_MODE

Date: 2026-05-18

## Status
Accepted

## Context
Operators have heterogeneous targets: solo Pi (standalone) vs DMZ cluster (cluster) vs future k8s (kubernetes).

## Decision
Single binary; `HUB_MODE` env var selects mode at startup. Mode-specific behaviour behind Store/Bus/Leader/Deduper/RateLimiter interfaces. `/api/cluster/{node,status}` reports active mode.

## Consequences
**Positive:** one binary, one CI pipeline. Operators upgrade modes without re-flashing.
**Negative:** interface boilerplate. Schema parity (Article XII) is harder to maintain across SQLite + MariaDB.

**Enforced by:** Article XVI.
