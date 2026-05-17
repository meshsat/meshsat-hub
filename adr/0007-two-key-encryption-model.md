# ADR-0007 — Two-key encryption model: static AES-256-GCM (hub↔device) + X25519 ECDH (bridge↔bridge)

* Status: Accepted — codified after the fact 2026-05-17. The model is committed in `internal/crypto/` and documented in `docs/ENCRYPTION.md`.
* Date: Originally decided during the v1.0 production-hardening phase; recorded as ADR 2026-05-17 during the deep-spec audit.
* Deciders: `ufwtqkgz@meshsat.net`
* Source document: `docs/ENCRYPTION.md`

## Context

MeshSat traffic has two profoundly different shapes:

1. **Field device → Iridium SBD/IMT → Hub** (and the reverse path). Satellite link is high-latency (30–90s store-and-forward), per-message-billed ($0.05+ per SBD/IMT), unidirectional in many real-world scenarios (modem broadcasts MO; MT delivery requires a separate provider API call), and infrequent (1 message every 5–15 min typical).
2. **Bridge ↔ Bridge over LoRa / Meshtastic / WiFi / ZigBee**. Low-latency (~100ms), free, bidirectional, frequent. Bridges act as routing peers in a mesh.

The same encryption scheme cannot serve both. A handshake-based protocol (ECDH-derived ephemeral session keys with forward secrecy) is the right answer for LoRa peers — but for satellite, a 3-packet handshake costs $0.15 just for key exchange and adds 2–5 minutes of latency across 3 satellite passes. That handshake cost would dominate every conversation.

Conversely, static pre-shared keys give up forward secrecy and mutual authentication — acceptable for a concentrator topology (Hub is not a routing peer, it's a sink/source for ≥1 satellite-attached device), unacceptable for a mesh of peer routers.

## Decision

Two distinct encryption models, each used for the path that suits it.

### Model 1 — Static per-device AES-256-GCM (hub ↔ device, satellite path)

- Hub generates a 256-bit AES key per device via `POST /api/devices/{imei}/keys`.
- Raw key returned ONCE in hex on the create response; stored hashed in Hub DB; copied to device (Bridge config or Android settings).
- Wire format: `[12-byte random nonce][ciphertext + 16-byte GCM auth tag]`. Random nonce per message (no nonce-reuse risk; GCM nonce-uniqueness is per-key).
- Properties:
  - AES-256-GCM authenticated encryption.
  - **No** forward secrecy — compromise of the device key decrypts all past messages encrypted with it.
  - **No** mutual authentication (the shared symmetric key is the authentication).
  - Simple: no handshake, works over unidirectional satellite.
- Rotation: operator-initiated via the Hub API. Create new key, update device config, delete old. Old messages encrypted with revoked keys are no longer decryptable.

### Model 2 — X25519 ECDH ephemeral session keys (bridge ↔ bridge, LoRa / WiFi / ZigBee)

- Each bridge holds a persistent Ed25519 signing key + X25519 encryption key as its **identity**.
- Link establishment is a 3-packet handshake totaling 259 bytes (fits in a single Iridium SBD as a worst-case carrier, though normally used over LoRa):
  1. `LinkRequest` (65B): type + dest_hash + ephemeral_pub + random
  2. `LinkResponse` (129B): type + link_id + ephemeral_pub + Ed25519 signature
  3. `LinkConfirm` (65B): type + link_id + proof
- Ephemeral X25519 keys per link → derived AES-256-GCM session keys.
- Links expire on inactivity timeout; re-established automatically; each new link has fresh ephemeral keys.
- Properties:
  - **Forward secrecy** — ephemeral keys per link; compromise of identity keys does not retroactively decrypt prior sessions.
  - **Mutual authentication** — both peers prove possession of their Ed25519 identity key.
  - Works over LoRa (fast, free, bidirectional). Unsuitable for satellite-only (handshake cost too high).

### When each applies

| Path | Model | Reason |
|---|---|---|
| Device → Iridium → Hub | 1 (static AES-256) | Satellite handshake too expensive |
| Device → Globalstar → Hub | 1 | Same |
| Hub → MQTT → Bridge | 1 | Hub is not a routing peer |
| Bridge ↔ LoRa ↔ Bridge | 2 (X25519 ECDH) | LoRa is fast, free, bidirectional |
| Bridge ↔ WiFi ↔ Bridge | 2 | Same |
| Bridge ↔ ZigBee ↔ Bridge | 2 | Same |
| Android ↔ BLE ↔ Meshtastic | neither (Meshtastic AES-CTR passthrough) | Not MeshSat's encryption — Meshtastic's wire format |

## Consequences

**Positive**
- Each path uses the right primitive for its constraints. The expensive choice (X25519 ECDH with handshake) is reserved for the low-cost link (LoRa).
- Operators can audit static-key rotation explicitly via the audit log; X25519 link establishment auto-logs handshake events.
- The crypto stack is pure-Go (`crypto/aes`, `crypto/cipher`, `crypto/ed25519`, `golang.org/x/crypto/curve25519`) — no CGO needed (consistent with Constitution Article V).

**Negative**
- Two systems to maintain, two threat models to reason about. The static-key path has **no forward secrecy** — operators must understand this when rotating keys after personnel change.
- Static-key compromise is catastrophic for historic traffic. Mitigation: quarterly rotation + rotate-on-personnel-change as operational policy (per `docs/ENCRYPTION.md` L77).

**Forward direction (MESHSAT-199)**
- When Hub adopts Reticulum as its network layer (planned), Hub will gain a Reticulum identity (Ed25519 + X25519 keypair) and participate in the announce/link protocol over MQTT — enabling Model 2 for Hub↔Bridge over the wired path. Static keys remain available as fallback for satellite-only devices that can't afford handshake cost.

## Alternatives considered

- **X25519 ECDH for everything (including satellite)**: rejected — handshake cost dominates conversation cost; 2–5 min handshake latency unacceptable for SOS-class events.
- **Static keys for everything (including bridge mesh)**: rejected — no forward secrecy in a peer-routing mesh is unacceptable; if a single bridge is captured, the entire mesh's history is decryptable.
- **WireGuard tunnels for all paths**: rejected — works for bridge-bridge over WiFi (and is in fact used at the network layer per `internal/wireguard/`), but doesn't help over LoRa/ZigBee/Iridium, and adds tunnel-setup latency.
- **Signal Protocol (X3DH + Double Ratchet)**: rejected for v1 — adds complexity and a third dependency for marginal forward-secrecy gain over per-link X25519 ECDH; revisit at v2 if peer-routing scale demands it.

## Compliance

- `internal/crypto/` MUST implement Model 1; key CRUD via `POST/GET/DELETE /api/devices/{imei}/keys`.
- RockBLOCK handler MUST attempt decryption when payload starts with a 12-byte nonce prefix (per `docs/EXECUTION_PLAN.md` L26–L32 — MESHSAT-169 wire-the-unwired).
- Key material MUST be redacted from logs, audit entries, and backup exports (per Constitution Article XIV).
- Constitution Article XIV's "ADR justification for every new third-party dep" rule applies — the X25519/Ed25519 implementation uses stdlib + `golang.org/x/crypto/curve25519` only.
