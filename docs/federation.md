# Hub federation: a satellite message to a kit on another Hub

`satellite_relay` (see `satellite-relay.md`) carries one modem's message to another modem byte for
byte, but only inside one tenant, because the Hub sends the MT on that tenant's own Cloudloop (IMT)
or Rock7 (SBD) account. Federation is the same relay between two tenants that each keep their own account: two
tenants on hub.meshsat.net, a tenant there and a self-hosted Hub, or two self-hosted Hubs
(MESHSAT-1549). Federation still delivers only to IMT modems; the SBD ends of 24 Oct 2026 are
`satellite_relay`'s alone.

## Trust

Each tenant has a federation identity: an Ed25519 key and a self-signed certificate, created the
first time the owner opens Settings > Tenant > Hub federation (`GET /api/federation/identity`).
The key lives in the tenant's encrypted provider row (provider `federation`) and never leaves it.

Nothing is trusted through a certificate authority. Owner A adds owner B's certificate and relay
URL; owner B adds A's. Both read their own fingerprint (SPKI SHA-256) on their page and compare it
with what the other one added, over a channel they trust. A Hub sends only to peers it holds and
accepts only from peers it holds, so traffic flows only when both owners have added each other.

## The wire

A peer posts to the other tenant's capability URL, `https://<hub>/api/webhook/federation/<token>`.
The token only names the tenant (`internal/webhookroute`, an unknown token is a 404); what
authorises the request is the signature:

| header | value |
|---|---|
| `X-MeshSat-Federation-From` | the sender's fingerprint |
| `X-MeshSat-Federation-To` | the receiver's fingerprint |
| `X-MeshSat-Federation-Date` | RFC 3339, within 5 minutes of the receiver's clock |
| `X-MeshSat-Federation-Signature` | Ed25519 over `meshsat-federation-v1`, method, path, both fingerprints, date and the SHA-256 of the body |

The body is `{"msg_id", "attempt", "from_imei", "to_imei", "imt_topic", "wire_b64"}`. The payload is
never decoded or re-encoded: the receiving kit authenticates the bytes with keys no Hub holds. The
signature covers the URL path as the sender posted it, so an ingress in front of a Hub must not
rewrite the path, or every relay to it is refused.

The receiver answers 404 for an unknown token, a sender that is not its peer, a request addressed to
another identity, or a signature that does not verify against the pinned certificate (the same
answer, so the URL alone does not reveal who the peers are); 403 when no grant lets that sender
reach that device or the device is not the tenant's; 502 when the satellite send failed; 200 for an
attempt it has already handled; 202 when the MT went out. It claims `msg_id` plus `attempt` once
across replicas (`store.ClaimOnce`) before sending, and sends nothing when the claim table is
unavailable. A replay of an attempt therefore sends nothing, and a retry after a failed send goes
out. A retry after a send whose answer was lost can deliver a second copy: the receiving Bridge
drops a repeated MT for 15 minutes and Reticulum drops a repeated packet, so that costs one message,
where claiming per message would lose one.

## Grants and routes

A grant on the receiving side lets one modem of a peer reach one of the tenant's own devices. Every
message it carries is an MT on the receiving tenant's account, so a grant is a standing spend
decision of its owner.

The sending side uses a route with the destination `federation_relay` and recipients written
`<peer id>/<imei>`, one route per direction, senders named, exactly as for `satellite_relay`:

| name | source | senders | destination | filter |
|---|---|---|---|---|
| kit A to the peer's kit | `iridium_imt:IMT_TOPIC_RAW` | `<IMEI of A>` | `federation_relay` | `<peer id>/<IMEI of B>` |

The route engine claims each dispatch once. The relay uses the publisher's message id as `msg_id`
(a digest of the payload when there is none), retries a 5xx or a timeout twice (2 s, 5 s) as
attempts 2 and 3, and treats a 4xx as final. A `federation_relay` route that names no senders, or
`*`, relays nothing: it would turn any modem's message into a paid MT on the peer. A peer may not
redirect a relay.

## Verifying it

Unit: `internal/federation` (signatures: forged signer, altered body, re-addressed, moved path,
stale and future dates; the relay between two services: once only, replay, a retry after a failed
send, one-sided peering, no grant, a device of another tenant, unknown token, no identity; two
replicas creating an identity at once), `internal/store/storetest` (`Federation`, the
default tenant as victim), `internal/routing` (`TestFederationRelayHandler`).

Live: two tenants each holding one 9704, peered and granted both ways; a Reticulum announce from one
kit appears in the other kit's `GET /api/rns/paths` on `iridium_imt_0`, and a packet with proof
crosses both ways.
