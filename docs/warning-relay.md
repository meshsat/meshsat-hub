# Last-mile warning relay (MESHSAT-1734)

The Hub reads the official public warning feeds and hands each warning, as a compact signed
digest, to the kits of every workspace that asked for it and is in the warning's area. A kit puts
the warning on its mesh (an alert and a text) and on its APRS bulletin channel, so people with a
Meshtastic node or an APRS radio hear it when the phone network does not reach them. This document
is the contract: the Hub side lives in `internal/warnings`, the Bridge side in the Bridge's
`internal/warnings`, and `docs/bridge-hub-protocol.md` "Public warnings" names the topic.

What it is not: an official warning channel. A relay is marked as one on every bearer and never
pretends to be NL-Alert, a cell broadcast or the sender's own app. The copy of the warning is the
sender's own headline, unchanged apart from being shortened to fit; an Update or Cancel from the
sender replaces or withdraws it on the kits the way it does at the sender.

## Feeds

Both feeds are official open data read without a key, by the leader replica, every
`HUB_WARNINGS_POLL_INTERVAL` (default 5 m).

| Source | id | URL (default) | What it is | Licence and attribution |
|---|---|---|---|---|
| `dwd` | 1 | `https://opendata.dwd.de/weather/alerts/cap/COMMUNEUNION_DWD_STAT/Z_CAP_C_EDZW_LATEST_PVW_STATUS_PREMIUMDWD_COMMUNEUNION_DE.zip` (`HUB_WARNINGS_DWD_URL`) | Deutscher Wetterdienst, a zip of one CAP 1.2 document per current warning (communal union level), replaced about every ten minutes; `LATEST` always names the newest | DWD open data under the German GeoNutzV terms: free use with the source named, "Quelle: Deutscher Wetterdienst". Each document also carries an `eventCode LICENSE` for the area geometry (BKG) |
| `meteoalarm-nl` | 2 | `https://feeds.meteoalarm.org/feeds/meteoalarm-legacy-atom-netherlands` (`HUB_WARNINGS_METEOALARM_NL_URL`) | MeteoAlarm (EUMETNET), the Netherlands: an Atom feed whose entries carry the CAP identifier and link the CAP document (KNMI is the sender) | The feed's own `<rights>`: "Licensed under terms equivalent to CC BY 4.0, with additional requirements for redistributing outlined in our Terms and Conditions." Those Terms are a page on meteoalarm.org that renders in the browser only; **read them before a production tenant relays MeteoAlarm**, and name MeteoAlarm and KNMI wherever the warning is shown |
| `test` | 255 | - | an owner's injection (`POST /api/warnings/test`) | - |

A fetch is bounded (8 MB, 60 s), follows no redirect, takes a CAP document only from the feed's
own host, and refuses a document with a DOCTYPE. A feed that fails is counted
(`meshsat_hub_warnings_fetched_total{source,result="failed"}`) and the others still run.

Every message is kept in `warnings` keyed (source, identifier), with its sent time and the hash of
the document: a re-poll that carries the same message changes nothing and delivers nothing; a
re-sent identifier (new `sent` or a changed document) is processed again. An Update or Cancel is
a message of its own whose `references` name the one it replaces. Rows outlive their last
sighting by 30 days.

## Who gets what

A workspace opts in under Settings, Integrations, "Public warnings (last-mile relay)"
(`integrations.ProviderWarnings`, fields `enabled`, `areas`, `languages`, `feeds`, `satellite`).
A warning goes to:

- **every kit of the workspace** when one of its `areas` lines is in the warning's area: a geocode
  as the feed names it (`EMMA_ID:NL804`, `WARNCELLID:501000005`, or the bare value, which matches
  any geocode name), or a circle `lat,lon,radius_km` whose centre lies inside a warning polygon or
  circle, or within the radius of a polygon vertex;
- **a kit on its own** when the kit's last reported position (its birth's location) lies inside a
  warning polygon or circle. A kit with no position never matches by position. A warning with
  geocodes only (DWD's) matches geocodes only: the Hub carries no cell geometry.

`languages` picks the `<info>` block (ISO 639-1, in order of preference; the first the warning
carries wins, else its first block). `feeds` limits the sources; empty takes all.

Delivery is once per (tenant, kit, source/identifier, message type) across the replicas
(`store.ClaimOnce("warn:<tenant>:<bridge>:<source>/<identifier>:<msgtype>")`), on the kit's own
`{ns}/bridge/{id}/warn/in`, QoS 1, never retained; the Hub alone may publish there
(`NATSPublishDeny`). With `satellite` true the same bytes also go to the kit's modem as an MT on
the workspace's own Cloudloop (IMT, raw topic, verbatim) or Rock7 (SBD) account, once per
(tenant, modem, message) under `warn:sat:...`; the modem is the device linked to the kit that
reported last. `satellite` is **off by default**: every warning in the area is then a paid message
per kit. The Hub's own kits are the default tenant and get satellite delivery only if that tenant
sets it.

Each match is recorded in `warning_matches` (tenant, source, identifier): `GET /api/warnings`
lists them for the console, newest first, with the kits and bearers.

## Digest v1

One warning in at most **234 bytes**: one Iridium SBD MT (270) with room, one Meshtastic packet.
The same bytes go on the topic and, when paid for, as the MT; the first byte doubles as the
**satellite MT type byte, `0x57`**, so the kit's modem gateway hands it over untouched (the SBD
gateway's own frames use 0x01 to 0x0F). Every integer is big-endian.

| off | len | field |
|---|---|---|
| 0 | 1 | magic `0x57` ('W'); the satellite MT type byte |
| 1 | 1 | version `0x01` |
| 2 | 1 | message type in the low 4 bits: 1 Alert, 2 Update, 3 Cancel; bit 7 set = test or exercise |
| 3 | 1 | source id (table above) |
| 4 | 8 | id: the first 8 bytes of SHA-256 of the CAP `identifier` |
| 12 | 8 | ref: the same of the identifier this Update or Cancel replaces (the last `references` entry); zero for an Alert |
| 20 | 4 | sent, Unix seconds |
| 24 | 4 | expires, Unix seconds; 0 when the warning names none |
| 28 | 1 | severity `<< 4` or urgency: severity 0 unknown, 1 Minor, 2 Moderate, 3 Severe, 4 Extreme; urgency 0 unknown, 1 Future, 2 Expected, 3 Immediate, 4 Past |
| 29 | 1 | certainty `<< 4` or category: certainty 0 unknown, 1 Unlikely, 2 Possible, 3 Likely, 4 Observed; category 0 Other, 1 Met, 2 Geo, 3 Safety, 4 Security, 5 Rescue, 6 Fire, 7 Health, 8 Env, 9 Transport, 10 Infra, 11 CBRNE |
| 30 | 1 | event: 0 other, 1 wind, 2 rain, 3 snow/ice, 4 thunderstorm, 5 fog, 6 heat, 7 cold, 8 coastal, 9 flood, 10 avalanche, 11 wildfire, 12 tornado (from MeteoAlarm `awareness_type`, DWD `GROUP`, else the event words) |
| 31 | 2 | language, ISO 639-1 in ASCII (`de`, `nl`, `en`); `00 00` unknown |
| 33 | 1 | area kind `<< 4` or count: kind 0 none (count 0), 1 geocodes (count 1..8), 2 polygon (count 3..12 points) |
| 34 | var | the area: a geocode is `[type 1][value 9 bytes ASCII, zero padded]` with type 0 other, 1 WARNCELLID, 2 EMMA_ID, 3 NUTS; a polygon point is `[lat int24][lon int24]`, degrees times 10 000, two's complement (11 m) |
| var | 1 | text length n (0..255) |
| var | n | text: the headline in the chosen language, SMAZ2 with the Meshtastic dictionary (`internal/compress`, byte for byte the Bridge's), shortened until the whole digest fits |
| end | 64 | signature over bytes `[0, end)`: ECDSA P-256 over SHA-256, `r` then `s`, 32 bytes each, zero padded |

The signing key is the **Hub's directory key** (`directory.TrustAnchor`, ES256), the one every
kit pins at provisioning (`dir_sign_pub`) or fetches once from `GET /api/relay/directory-key`
and reports as `directory_key_id` in its birth. `GET /api/warnings/key` answers it as PKIX hex. A
kit refuses a digest it cannot verify with its pinned key, so a rotation of the directory key
reaches the relay the way it reaches snapshots.

Geocodes win over a polygon: a digest carries the first eight geocodes, else the largest polygon
simplified (Douglas-Peucker, growing tolerance) to twelve points. A geocode longer than nine
ASCII characters is left out.

### Test vector

Test key, P-256 private scalar `1f2d3c4b5a69788796a5b4c3d2e1f00112233445566778899aabbccddeeff001`;
its public key, PKIX DER:

```
3059301306072a8648ce3d020106082a8648ce3d03010703420004
8255ccf3e18d398e17a3aa927965387335c09fcea6e05c7671f134b2f6b4a799
588325ca016280e097d515e21218b6528bf1b7cd8ddde016baee3cecc2c95445
```

The warning: Alert, source 2 (meteoalarm-nl), identifier
`2.49.0.0.528.0.NL.20261028101141.6ddd5a99-421a-4c94-b9ea-25fa0ddcdd48_2`, sent
2026-10-28T10:11:41Z, expires 2026-10-29T10:11:41Z, Moderate / Expected / Observed / Met, event
wind, language `nl`, one geocode EMMA_ID `NL804`, text "Kans op windstoten". The digest, 120 bytes:

```
57 01 01 02                                              magic, version, Alert, source 2
f161d743dd14e974                                         id
0000000000000000                                         ref (an Alert)
6ae1ca5d 6ae31bdd                                        sent, expires
22 41 01 6e6c                                            sev 2/urg 2, cert 4/cat 1, event 1, "nl"
11 02 4e4c383034 00000000                                1 geocode: EMMA_ID "NL804"
0b 4b857320e820dc918cbb8d                                11 bytes of SMAZ2 text
f76c877ecedb20f0442bf905ec8e55b41bc8a7701b47892201770a5e189e163e   r
2d81e77d1bfe52e766d5c90c2748cdcc0d31ccd360eb731e8e2fc75b4eba865b   s
```

The body (the first 56 bytes) is what `Encode` writes for that warning; the signature is one of
the test key's (ECDSA is randomised, so `Encode` makes a different valid one each time). The Hub's
`TestDocumentedVector` and the Bridge's decoder test both hold these bytes; `WARN_VECTOR=1 go test
./internal/warnings -run TestPrintVector -v` regenerates them after a change of layout, which is a
change of this contract.

## What the kit does

On its `warn/in` (and on a satellite MT starting `0x57`) the kit verifies the signature with its
pinned directory key and refuses anything else, logged. It keeps the warning by its id (SQLite
`warnings`), never relays the same (id, message type) twice, replaces the row an Update refers to
and marks cancelled the one a Cancel refers to. Then, once per bearer, through its delivery
ledger:

| Bearer | What goes out |
|---|---|
| mesh, port 11 (ALERT_APP) | `WARNING <event> <severity> until <HH:MM UTC>: <text>` + newline + the label, then BEL; a node with its alert setting on buzzes |
| mesh, text broadcast on the primary channel | the same text without BEL, for nodes without alert settings; one message, not one per node |
| APRS, `BLN<d>` bulletin (d = the last id byte mod 10) | at most 67 characters: `<EVENT> <SEV> til <HH:MM>: <text> -MeshSat relay`, when the APRS gateway runs |

A test (bit 7) is prefixed `TEST`; a Cancel sends `CANCELLED: <event>` with the label; an Update
sends `UPDATE: ` and the new text. The label on the mesh is
**"Relay of `<source>` warning, issued `<time>` via MeshSat, not an official NL-Alert"**, where
the source is `DWD`, `KNMI via MeteoAlarm` or `MeshSat test`; the APRS form is cut to
` -MeshSat relay`. An expired warning (expires in the past) is stored and not relayed.
`GET /api/warnings` on the kit lists what it holds.

## API (Hub)

| Route | Role | Answers |
|---|---|---|
| `GET /api/warnings` | viewer | the warnings that matched this workspace and went to its kits, newest first |
| `GET /api/warnings/key` | viewer | `{"enabled","alg":"ES256","pkix_hex"}`, the key the kits verify with |
| `POST /api/warnings/test` | owner | the body is a CAP 1.2 document with `<status>Test</status>` or `Exercise`; it runs the same parse, match, sign and deliver path for this workspace alone and answers which kits matched and what each was handed (`deliveries[].bearers`, `digest_hex`). Audit `warning_test_injected` |

A test injection with `satellite` on spends on the workspace's own satellite account like a real
warning would.

## Configuration

| Variable | Default | |
|---|---|---|
| `HUB_WARNINGS_ENABLED` | `false` (`true` in `k8s/hub/configmap.yaml`) | the switch; off, nothing is fetched and the API answers `enabled: false` |
| `HUB_WARNINGS_POLL_INTERVAL` | `5m` (at least 1m) | |
| `HUB_WARNINGS_DWD_URL` | the DWD zip above; empty turns the feed off | |
| `HUB_WARNINGS_METEOALARM_NL_URL` | the MeteoAlarm NL Atom feed; empty turns it off | |

Metrics: `meshsat_hub_warnings_fetched_total{source,result}` (poll, failed, message, new),
`meshsat_hub_warnings_matched_total{source}`,
`meshsat_hub_warnings_delivered_total{bearer,result}` (mqtt and satellite; sent, failed,
claimed_elsewhere, no_modem).

## Not in this version

A geocode-to-geometry table (a DWD cell matches a kit only by the geocode the tenant names); more
feeds (GDACS, USGS, NINA, NL-Alert, which has no public feed); a repeat of a warning on the mesh
while it is in force; the apps; the 2G cell's broadcast channel on the kit; the legal opinion on
relaying under EECC article 110.
