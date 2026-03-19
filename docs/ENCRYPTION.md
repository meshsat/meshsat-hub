# MeshSat Encryption Architecture

## Two Key Models

MeshSat uses two distinct encryption models depending on the communication path:

### 1. Static Per-Device Keys (Hub ↔ Device)

**Used for:** All satellite traffic (Iridium SBD, Astrocast) between field devices and Hub.

**Key management:**
- Hub generates AES-256 key via `POST /api/devices/{imei}/keys`
- Key shown once on creation (hex string), stored hashed in Hub DB
- Same key copied to device (Bridge config or Android settings)
- Key persists until revoked

**Wire format:** `[12-byte random nonce][ciphertext + 16-byte GCM auth tag]`

**Properties:**
- AES-256-GCM authenticated encryption
- Random nonce per message (no nonce reuse risk)
- No forward secrecy (compromised key decrypts all past messages)
- Simple: no handshake needed, works over unidirectional satellite

**Why not X25519 for Hub ↔ Device?**
- Satellite SBD is store-and-forward with 30-90 second latency
- A 3-packet ECDH handshake would cost 3× satellite messages ($0.15) just for key exchange
- The handshake would take 2-5 minutes (3 satellite passes)
- Hub is not a routing peer — it's a concentrator that receives from many devices
- Static keys are the correct model for satellite concentrator architecture

### 2. X25519 ECDH Links (Bridge ↔ Bridge)

**Used for:** Peer-to-peer mesh routing between bridges over LoRa/Meshtastic.

**Key management:**
- Each bridge has a persistent Ed25519 signing key + X25519 encryption key
- Link establishment: 3-packet handshake (Request → Response → Confirm)
- Ephemeral X25519 keys per link → derived AES-256-GCM session keys
- Links expire and re-establish automatically

**Handshake (259 bytes total, fits in single Iridium SBD):**
1. LinkRequest (65B): type + dest_hash + ephemeral_pub + random
2. LinkResponse (129B): type + link_id + ephemeral_pub + Ed25519 signature
3. LinkConfirm (65B): type + link_id + proof

**Properties:**
- Forward secrecy (ephemeral keys, new keys per link)
- Mutual authentication (Ed25519 signatures)
- Works over LoRa (low latency, free, bidirectional)
- NOT suitable for satellite (too slow, too expensive for handshake)

### When Each Model Applies

| Path | Key Model | Why |
|------|-----------|-----|
| Device → Iridium → Hub | Static AES-256 | Satellite is slow + expensive for handshake |
| Device → Astrocast → Hub | Static AES-256 | Same reason |
| Device → Globalstar → Hub | Static AES-256 | Same reason |
| Bridge → LoRa → Bridge | X25519 ECDH | LoRa is fast, free, bidirectional |
| Bridge → WiFi → Bridge | X25519 ECDH | Same reason |
| Bridge → ZigBee → Bridge | X25519 ECDH | Same reason |
| Hub → MQTT → Bridge | Static AES-256 | Hub is not a routing peer |
| Android → BLE → Meshtastic | Meshtastic AES-CTR | Passthrough, not MeshSat's encryption |

### Future: Hub as Reticulum Transport Node (MESHSAT-199)

When Hub adopts Reticulum as its network layer, Hub will get a Reticulum identity
(Ed25519 + X25519 keypair) and participate in the announce/link protocol. This will
enable X25519 links between Hub and bridges over MQTT, but static keys will remain
available as a fallback for satellite-only devices that can't afford the handshake cost.

## Key Rotation

### Static Keys
- Rotate via Hub API: create new key, update device config, delete old key
- Old messages encrypted with revoked key cannot be decrypted
- Recommended: rotate quarterly or after personnel change

### X25519 Links
- Automatic: links expire after inactivity timeout
- New ephemeral keys generated per link establishment
- No manual rotation needed

## Security Comparison

| Property | Static AES-256 | X25519 ECDH |
|----------|---------------|-------------|
| Forward secrecy | No | Yes |
| Mutual authentication | No (shared secret) | Yes (Ed25519 signatures) |
| Handshake cost | 0 messages | 3 messages |
| Handshake latency | 0 | ~100ms (LoRa), ~3min (satellite) |
| Key compromise impact | All past messages | Only current link session |
| Suitable for satellite | Yes | No (too expensive) |
| Suitable for mesh | Yes (but no forward secrecy) | Yes (preferred) |
