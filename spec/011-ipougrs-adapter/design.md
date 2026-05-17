# Design — IPoUGRS Adapter (alpha)

## Goal

Per `docs/ROADMAP.md` L150, IPoUGRS is the "experimental differentiator" that lets thin IP traffic ride the Iridium SBD bearer. The Hub-side adapter ingests IPoUGRS-encoded MO, decodes to an IP packet, and emits to a local TUN interface (`meshsat-ipougrs0`). Reverse direction: REST API accepts an IP packet, encodes, sends via Cloudloop MT.

**Status: alpha.** Not on the v1.x critical path. The wire format is intentionally version-stamped (REQ-1012) so we can iterate before v2.0 ships generally.

## Wire diagram

```
                         (inbound)
       MO from RockBLOCK/Cloudloop
              │
              ▼ first byte == 0x49 ?
       ┌──────────────────┐  no  → existing handler
       │ ipougrs.Decode() │
       └────────┬─────────┘  yes
                │
                ▼
        IP packet bytes
                │
                ▼
       /dev/net/tun (meshsat-ipougrs0)
                │
                ▼
       downstream IP-aware tooling
       (Linux routing, packet capture, etc.)


                         (outbound)
       POST /api/ipougrs/send
       { destination_imei, payload: base64(ip_packet) }
              │
              ▼
       ipougrs.Encode(ip_packet)
              │
              ▼
       prepend 0x49 + 0x01 magic+version
              │
              ▼
       cloudloop.Client.SendMT(imei, encoded)
```

## Codec details

The exact encoding is intentionally underspec'd in this spec — implementation can choose between:
- Naive `0x49 0x01 <ip-packet-bytes>` (simplest; 2-byte overhead).
- Length-prefixed `0x49 0x01 <varint-length> <ip-packet-bytes>` (clearer framing).
- Compressed `0x49 0x01 <smaz2(ip-packet)>` (smaller wire bytes; reuses existing SMAZ2 from Constitution Article VI).

The reference implementation should pick (a) for v0.1, iterate to (c) before v2.0 GA. Wire format version byte (REQ-1012) lets receivers reject unknown versions cleanly.

## TUN interface management (REQ-1003 + REQ-1004)

Creating a TUN interface requires `CAP_NET_ADMIN`. The Hub container may or may not have it. Behavior:
- `HUB_IPOUGRS_ENABLED=true` + `CAP_NET_ADMIN` present → create TUN, full operation
- `HUB_IPOUGRS_ENABLED=true` + `CAP_NET_ADMIN` absent → log ERROR, mark feature inert, do NOT crash
- `HUB_IPOUGRS_ENABLED=false` (default) → no TUN creation, no error

The "do not crash" rule (REQ-1004) is critical for production deployments that toggle this experimentally — a missing capability should not take down the Hub.

## Mode gating (REQ-1011)

Like the simulator (spec/010), default-off in cluster mode + opt-in via explicit env var with startup WARN. Cluster mode is production; experimental features need operator intent.

## Out of scope

- No automatic Cloudloop MT scheduling — operator controls send timing via the REST endpoint.
- No IP-to-IMEI routing table (e.g., "10.0.0.42 → IMEI 30...") — single-IMEI per send call; operator-mapped externally.
- No NAT or stateful IP processing — encoder/decoder only.
- No IPv6 in alpha — IPv4 only.
