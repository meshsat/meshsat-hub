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

Unframed, a first byte `0x01` counts as Reticulum only when the packet verifies as a signed
announce (destination hash and signature): as RNS flags `0x01` is an announce and nothing else,
and a kit's message without base64 is the version byte followed by binary that passes the
header shape tests about one time in 24 once it is 167 bytes or longer (hops below 128, a defined
context byte). Behind `RNSI\x01` the shape still decides.

A kit's own message (not Reticulum) is the version byte followed by its egress chain's output. The
version byte says a chain ran, not that it encrypted: a plain SMAZ2 chain's output is binary too,
and no length or printability test tells it from ciphertext. So the Hub takes such a payload it
cannot decrypt for ciphertext only when:

- it is base64 of binary (the kits' `smaz2, encrypt, base64` chain), as the Cloudloop path always
  did; it keeps that base64 as its `text`;
- or the bridge that owns the modem declared the chain sealing (`egress_format: sealed` on the
  interface with that `imei` in its birth, `docs/bridge-hub-protocol.md`), as a kit does once its
  satellite chain encrypts without base64 (HeMB-NG S3-2). Its `text` is empty;
- or the Hub could not read the declaration (a database error) and has none confirmed for that
  modem since it started: a modem that may be sealing is never guessed at. Its `text` is empty and
  the Hub logs the failed lookup.

Either way it is published `opaque: true`, keeps its wire, and never goes through the SMAZ2 guess,
which turned ciphertext into printable nonsense. A modem without a declaration, or declared
`plain`, is read exactly as before, plain SMAZ2 included. The same rule runs on the Rock7 (SBD) and
Globalstar webhooks, and anything else the Hub cannot turn into text is opaque too, so no route
shows a person an empty message.

The declaration is the modem's, kept in the Hub's database (`devices.egress_format`, with the
bridge that made it): the birth subscriber writes it when a birth names the modem's `imei` with a
format, and nothing else changes it. A birth without the `imei` or without the gateway leaves it,
so a restart before the 9704 has told its IMEI does not undo it. An explicit `plain` replaces
`sealed`. It applies while the bridge that made it owns the modem; a modem that moves to another
bridge is read by that bridge's declaration, or as undeclared. The reader falls back to the
owner's last birth (a birth stored before the record existed) and, when the database cannot be
read, to what it last confirmed for the modem.

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

Unit: `internal/cloudloop/webhook_reticulum_test.go`, `internal/cloudloop/webhook_s32_test.go`,
`internal/cloudloop/review_r26_test.go`, `internal/codec/opaque_test.go`,
`internal/bridge/egress_test.go`, `internal/bridge/review_r27_test.go`,
`internal/cloudloop/review_r27_test.go`, `internal/store/storetest/device_egress.go`, `internal/rockblock/handler_s32_test.go`,
`internal/globalstar/handler_s32_test.go`, `internal/routing/opaque_routes_test.go`,
`internal/routing/engine_test.go`
(`TestMatchSourceTopic`), `internal/routing/destinations_test.go`
(`TestSatelliteRelayForwardsTheWireUntouched`), `internal/protocol/hemb_test.go` (a CRC-8
coincidence is no longer a HeMB frame; about one in 256 arbitrary payloads used to vanish as a
"HeMB symbol" before routing).

Live: with both kits' 9704 modems under open sky, one Reticulum announce from kit A must appear
in kit B's `GET /api/rns/paths` with interface `iridium_imt_0`, and a packet with proof must
cross both ways. Read `docs/RUNBOOK.md` for the Cloudloop side; the kits' side is in the Bridge
repository (`MESHSAT-1352`).
