# Kit to kit over satellite: the `satellite_relay` destination

The Hub can carry one modem's satellite message to another modem **byte for byte**. That is how
two MeshSat kits with RockBLOCK 9704 modems exchange Reticulum packets when neither has any
other bearer: the kit's `iridium_imt_0` interface puts a raw Reticulum packet in an IMT message,
Cloudloop posts it to the Hub's webhook, a route relays the original bytes to the other modem,
and the other kit's `iridium_imt_0` hands the packet to its Reticulum node. The same path carries
a message from a RockBLOCK 9704 running CrossTalk's `IridiumIMTInterface` (its `RNSI` framing
is passed through untouched; the kit's interface strips it).

This is distinct from the `satellite` destination, which sends the decoded **text** with a
sender prefix for a person to read. Anything the Hub adds or re-encodes makes a relayed packet
undeliverable: the receiving kit authenticates the bytes with keys the Hub does not hold.

## What arrives on `mo/decoded`

Every IMT message carries `imt_topic` (`IMT_TOPIC_RAW`, `IMT_TOPIC_PURPLE`, ...). A message on
`IMT_TOPIC_RAW` whose bytes have the shape of a Reticulum packet (RNS 1.5 header, optionally
behind CrossTalk's `RNSI\x01` header) is published with `reticulum: true`, `opaque: true`, an
empty `text`, and `wire` holding the payload exactly as received. It skips the protocol
version-byte strip, decryption and decompression: an RNS announce starts with `0x01`, which is
also the Bridge's version byte, and classifying its bytes as text would route ciphertext-like
noise to people. Only destinations that can do something with an opaque payload see it
(`satellite_relay`, `webhook`, `mqtt`, `notification`).

## The two routes

One route per direction, each naming its sender and its recipient:

| name | source | senders | destination | filter |
|---|---|---|---|---|
| kit A to kit B | `iridium_imt:IMT_TOPIC_RAW` | `<IMEI of A>` | `satellite_relay` | `<IMEI of B>` |
| kit B to kit A | `iridium_imt:IMT_TOPIC_RAW` | `<IMEI of B>` | `satellite_relay` | `<IMEI of A>` |

- The source form `<source>:<IMT topic>` pins the route to one topic; `iridium_imt` alone matches
  any IMT message; `satellite` and `*` keep their old meaning. A route pinned to a topic never
  fires on SBD or SMS traffic.
- The relay answers on the topic the message arrived on, so a packet sent on `IMT_TOPIC_RAW`
  lands on the other kit's raw topic, which is what its interface listens to.
- The relay never sends back to the origin, and a verbatim payload is never fragmented: a 9603
  (SBD, 270 bytes) cannot be a relay target.
- **Name the senders.** Every relayed message is a paid MT. A relay open to any sender forwards
  whatever any modem on the account transmits.
- Keep the default `satellite` fan-out routes (TAK, APRS, SMS, notification) restricted by lane or
  sender for these IMEIs; a wildcard text route would still try to render an opaque payload where
  it is allowed to (`notification`), and a person would receive noise.

## Verifying it

Unit: `internal/cloudloop/webhook_reticulum_test.go`, `internal/routing/engine_test.go`
(`TestMatchSourceTopic`), `internal/routing/destinations_test.go`
(`TestSatelliteRelayForwardsTheWireUntouched`), `internal/protocol/hemb_test.go` (a CRC-8
coincidence is no longer a HeMB frame; about one in 256 arbitrary payloads used to vanish as a
"HeMB symbol" before routing).

Live: with both kits' 9704 modems under open sky, one Reticulum announce from kit A must appear
in kit B's `GET /api/rns/paths` with interface `iridium_imt_0`, and a packet with proof must
cross both ways. Read `docs/RUNBOOK.md` for the Cloudloop side; the kits' side is in the Bridge
repository (`MESHSAT-1352`).
