# Kit to kit over satellite: the `satellite_relay` destination

The Hub can carry one modem's satellite message to another modem **byte for byte**. That is how
two MeshSat kits with RockBLOCK 9704 modems exchange Reticulum packets when neither has any
other bearer: the kit's `iridium_imt_0` interface puts a raw Reticulum packet in an IMT message,
Cloudloop posts it to the Hub's webhook, a route relays the original bytes to the other modem,
and the other kit's `iridium_imt_0` hands the packet to its Reticulum node. The same path carries
a message from a RockBLOCK 9704 running CrossTalk's `IridiumIMTInterface` (its `RNSI` framing
is passed through untouched; the kit's interface strips it).

Since 24 Oct 2026 either end can also be a RockBLOCK 9602/9603 (SBD) on Rock7, as a field kit's
RockBLOCK Mk2 is: its MO keeps its `wire` like an IMT one, and a relay to it goes out through the
tenant's Rock7 account (see "SBD ends" below). [MESHSAT-1623]

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

The declaration is kept per bridge and modem (`bridge_egress`), with the stamp of the birth that
made it: the birth's own `timestamp`, which a Bridge never lets go backwards. A copy per modem
(`devices.egress_format`, with the bridge that made it) is written after it and always reads the
bridge's stored declaration, so it can never be older than that. Only a birth that names the
modem's `imei` with a format changes them. A birth without the `imei`, without the gateway, or with
the `imei` and no format (a Bridge that could not read its chain) leaves them, so a restart before
the 9704 has told its IMEI does not undo them. An explicit `plain` replaces `sealed`. They apply
while the bridge that made them owns the modem; a modem that moves to another bridge is read by
that bridge's declaration, or as undeclared.

Every part of a birth is written in one transaction and is ordered, so writing a birth later than
a newer one (a retry, or a replica that is behind) changes nothing the newer one stored:

- the bridge row and each declaration only when the birth's stamp is not older than the stored
  one;
- the modem's link to a bridge only when the link on record was made earlier: a link carries the
  time the Hub received the birth that made it, a delete of the bridge stamps its modems' links
  with the time of the delete, so a retry cannot take a modem a later birth moved or link it to a
  deleted bridge again.

A birth the store has not taken is held by the subscriber, merged with any later birth of that
bridge (the newest row, per modem the newest declaration and the latest link), and written again
after every birth the store takes and every 30 s. It is let go only once the store has taken it,
whether its parts were written or found older than what was stored. The readers in that process
take a held declaration first, unless the bridge's stored one is newer (another replica wrote a
later birth); a held birth that links the modem to a bridge other than its owner is unresolved
until written, and the payload is kept opaque. A Hub that restarts first reads the bridge's
retained birth on subscribe. After the held declaration, the reader's order is: the owner's stored
declaration, the per-modem record, the owner's last birth (one saved before the declarations
existed), and, when the database cannot be read, a `sealed` it last confirmed for the modem. While
the database cannot be read, only a `sealed`, held or confirmed, answers: a `plain`, held or
confirmed, does not, since another replica may have stored a newer `sealed` meanwhile, and the
payload is kept opaque until the database answers again.

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
- The relay never sends back to the origin, and a verbatim payload is never fragmented: no Bridge
  or phone reassembles the Hub's fragment header.
- **SBD ends.** An SBD MO arrives through the Rock7 webhook and is published with `wire` (the payload
  as sent, version byte included) and no `imt_topic`, so a route pinned to an IMT topic never fires
  on it; use the `satellite` source. A relay to a modem the tenant registered as `rockblock` or
  `iridium_sbd` goes out through the tenant's Rock7 account as one MT of at most 270 bytes (a
  9602/9603's MT frame); a longer payload is refused, never cut. Each such MT is stored as an `mt`
  message (`channel` `iridium`, `raw_hex` the bytes) and published on the modem's `mt/status`
  (`queued` with Rock7's id, or `failed` with the reason). An SBD message relayed to an IMT modem
  goes on the sender's default topic, `IMT_TOPIC_RAW`, which is the one the Bridge and MeshSat
  Android use for the satellite chain. A modem that is neither a known IMT device nor a registered
  Rock7 one is refused, as is any other device type.
- **Name the senders.** Every relayed message is a paid MT. A relay open to any sender forwards
  whatever any modem on the account transmits.
- Keep the default `satellite` fan-out routes (TAK, APRS, SMS, notification) restricted by lane or
  sender for these IMEIs; a wildcard text route would still try to render an opaque payload where
  it is allowed to (`notification`), and a person would receive noise.

## Verifying it

Unit: `internal/cloudloop/webhook_reticulum_test.go`, `internal/cloudloop/webhook_s32_test.go`,
`internal/cloudloop/review_r26_test.go`, `internal/codec/opaque_test.go`,
`internal/bridge/egress_test.go`, `internal/bridge/egress_r29_test.go`, `internal/bridge/egress_r30_test.go`,
`internal/bridge/review_r27_test.go`, `internal/cloudloop/review_r28_test.go`, `internal/cloudloop/review_r29_test.go`,
`internal/cloudloop/review_r27_test.go`, `internal/store/storetest/device_egress.go`, `internal/rockblock/handler_s32_test.go`,
`internal/globalstar/handler_s32_test.go`, `internal/routing/opaque_routes_test.go`,
`internal/routing/engine_test.go`
(`TestMatchSourceTopic`), `internal/routing/destinations_test.go`
(`TestSatelliteRelayForwardsTheWireUntouched`), `internal/satrelay/sender_test.go` (the provider per
recipient, the 270-byte SBD frame, refusals), `internal/rockblock/handler_sbd_relay_test.go` (an SBD
MO's `wire`, through the real relay handler), `internal/cloudloop/webhook_reticulum_test.go`
(`TestWebhook_ACRC8CoincidenceIsNotAHeMBFrame`: a packet whose byte 7 is the CRC-8 of bytes 0-6
is routed; about one in 256 arbitrary payloads used to vanish as a "HeMB symbol" before routing,
and the HeMB check left the Hub in MESHSAT-1763).

Live: with both kits' 9704 modems under open sky, one Reticulum announce from kit A must appear
in kit B's `GET /api/rns/paths` with interface `iridium_imt_0`, and a packet with proof must
cross both ways. Read `docs/RUNBOOK.md` for the Cloudloop side; the kits' side is in the Bridge
repository (`MESHSAT-1352`).
