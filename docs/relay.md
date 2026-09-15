# WebSocket relay (MESHSAT-612)

The Hub side of the fallback path for a bridge that cannot be reached directly: behind carrier
NAT, on a phone, or on a bearer with no inbound port. The bridge opens one outbound WebSocket to
the Hub and serves its clients through it; a client opens its own WebSocket to the Hub naming
the bridge; the Hub joins the two. This document is the contract the Bridge (MESHSAT-613) and
the Android app build against.

## What the Hub is, and is not

The Hub carries bytes between two sockets and reads none of them. The two ends negotiate their
own mTLS session **inside** the tunnel with the certificates the Hub already issues to bridges,
so a compromised Hub can withhold or delay traffic but cannot read or forge it. Nothing that
crosses the relay is stored, logged beyond a count, or retained on the bus.

## Identity

Both ends are bridges of one tenant. An Android device is a bridge of type `android`. Each
authenticates with the MQTT credentials it already holds, as **HTTP Basic** on the upgrade
request: username = bridge id, password = the MQTT password shown once when credentials were
generated (Fleet page, or `POST /api/bridges/{id}/credentials`). The Hub verifies it with bcrypt
against the stored hash. The tenant is the bridge's owner in the store; nothing in the request
chooses it.

The client's id on the other end is **its own bridge id**, never a parameter. That is what makes
per-client isolation hold by construction: a client cannot name itself as somebody else.

## Endpoints

| | who | what |
|---|---|---|
| `GET /api/relay/serve` | the bridge | one socket, every client of that bridge multiplexed |
| `GET /api/relay/connect/{bridge_id}` | a client | one socket, one tunnel to that bridge |

Answers before the upgrade: `401` bad or missing credentials (`WWW-Authenticate: Basic
realm="meshsat-relay"`), `403` the target is not a bridge of the caller's tenant (the same
answer for a bridge that does not exist), `400` a client naming itself, `429` the client's
budget for this minute is spent. `101` otherwise.

Both routes are outside the user auth middleware and carry the webhook-style per-IP upgrade
budget (60 per minute per source address).

## Frames

**Binary frames only.** A text frame closes the socket with `1003`.

On the **client** socket a frame is the bare payload, both directions.

On the **bridge** socket every frame carries an envelope, both directions:

```
[0x01][len u8][client_id][payload]
```

`len` is the byte length of `client_id` (1..255). An envelope that does not parse closes the
bridge socket with `1003`. The largest frame accepted from either end is 64 KiB.

## Keepalive

The Hub pings every 30 s and closes a socket that has shown no sign of life (a pong or any
frame) for 60 s. A conforming client library answers pings by itself as long as it is reading.

## Budget

100 frames per minute per client id, counted on the untrusted end: the client's frames and its
connect attempts. The bridge's replies are not counted. Exhausted before the upgrade is `429`;
exhausted mid-session closes the client socket with `1008` and a reason. The window is a
calendar minute shared across replicas (Redis), so reconnecting to another replica does not
reset it.

## Close codes

| code | meaning |
|---|---|
| `1000` | the Hub is shutting the socket normally (its own restart, the read loop ended) |
| `1001` | superseded: the same identity opened a newer socket on this replica |
| `1003` | a frame the Hub cannot accept (not binary, bad envelope) |
| `1008` | budget exhausted for this minute |

A client whose bridge is not connected anywhere is not told so: its frames are published and
nobody picks them up. It learns by the absence of replies inside its own protocol. A presence
signal is the Bridge's to add if it wants one (MESHSAT-613).

## Inside the tunnel

The Hub carries bytes. What the two ends put in them is **TLS, with the Hub CA as the only
root**, and inside the TLS stream plain HTTP/1.1 to the bridge's own API, so the pair
protocol's Bearer tokens keep working unchanged.

- The **bridge end is the TLS server** of every tunnel: it presents its Hub-issued
  certificate and requires the client's Hub-issued certificate (`RequireAndVerifyClientCert`,
  roots = Hub CA). Its relay client turns every client id into one `net.Conn` and serves the
  API router over `tls.NewListener`.
- The **client end** (a phone) presents its Hub-issued certificate and verifies the server
  against the Hub CA with `ServerName` = the bridge id it connected to. It exposes the tunnel
  as a local port, so its HTTP stack does TLS the normal way against `https://127.0.0.1:<port>`.
- **Certificates issued before 2026-09-15 cannot serve a relay**: they carried only
  `ClientAuth` and no SAN. `IssueBridgeCert` now adds `ServerAuth` and a DNS SAN equal to the
  bridge id (ids that are not DNS names get no SAN and cannot serve). A bridge re-issues its
  certificate on the Fleet page once; phones need nothing new, `ClientAuth` was always there.
- Frames are at most 64 KiB, so both ends chunk the TLS stream at 32 KiB per frame.

## How the two ends meet

Three Hub replicas sit behind round-robin, so the two ends of a tunnel land on different pods.
The join is a rendezvous over the message bus, not a map on one pod:

```
meshsat/{tenant}/relay/{bridge}/{client}/up     client -> bridge
meshsat/{tenant}/relay/{bridge}/{client}/down   bridge -> client
```

(`meshsat/relay/...` for the default tenant, like every other topic family.) Every replica
subscribes to the four filters once at startup and dispatches from its own in-memory sessions;
a replica holding neither end ignores the frame. Plain subscriptions, not a queue group: the
replica holding the socket must be the one that receives. Both ids are percent-encoded on the
wire (`internal/mqtt.EncodeSegment`).

## Metrics

`meshsat_hub_relay_sessions{role}` (bridge, client) per replica;
`meshsat_hub_relay_frames_total{direction}` (up, down) accepted from a socket;
`meshsat_hub_relay_rejected_total{reason}` (auth, tenant, budget, frame, envelope, upgrade).

## Edge

Nothing at the edge changed. The VPS HAProxy carries `/` in HTTP mode with `timeout tunnel
3600s` and ingress-nginx has `proxy-read-timeout 3600`, so an idle tunnel lives an hour at the
edge and the Hub's own ping keeps it from ever being idle that long. Both hops terminate TLS,
which is why the mTLS session has to live inside the tunnel rather than on it.

## Trying it

```bash
# bridge end (kit-a, its MQTT password)
websocat -b --basic-auth 'kit-a:PASSWORD' wss://hub.meshsat.net/api/relay/serve

# client end (phone-1, its own MQTT password)
websocat -b --basic-auth 'phone-1:PASSWORD' wss://hub.meshsat.net/api/relay/connect/kit-a
```

`scratchpad/relay-live.py` is the acceptance run: two throwaway bridges of the test tenant, a
frame each way with the envelope checked, run twice so the ends land on different replicas.
