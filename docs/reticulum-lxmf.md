# The Hub as a Reticulum node: tenant LXMF addresses, inbox and outbox

Since MESHSAT-1555 the Hub runs an upstream-compatible Reticulum node and gives every tenant an
LXMF address on it. A Sideband, CrossTalk, MeshChat or NomadNet user messages that address; the
message lands in the tenant's inbox in the console and the API, and what the tenant sends is
marked delivered when the peer proves it. The implementation is the Bridge's, verified against
Python RNS 1.5.5 and LXMF 1.2.0 (`internal/rnswire`, `internal/rns`, `internal/msgpack`,
`internal/lxmf`); the Hub-specific parts are `internal/rnsnode` (the node on the Hub's
interfaces) and `internal/lxmfhub` (the tenants' addresses).

## One node, on the leader

Links, path tables and TCP sessions are stateful and the Hub runs two replicas, so the node runs
on the leader only: `internal/rnsnode.Service` starts with leadership and stops on loss, like
APRS-IS. Its transport identity is the Hub's existing Reticulum identity (`system_config`), so the
transport id and the `meshsat.hub` destination hash the kits know did not change.

**The hub-rns Service follows the leader.** The leader labels its own pod
`meshsat.net/rns-leader=true` (`internal/leader.PodLabeler`, cleared at every start and on
loss), the Service `hub-rns` selects on that label, and stunnel connects to `hub-rns:4242`. A
Service that balanced across both pods would refuse half of all connections. On a failover the
label moves within the lease time (about 15 s) and clients reconnect by themselves.

Paths persist in `rns_paths` with the cached announce, so the leader that takes over answers path
requests at once; expired rows are forgotten on load. A TCP connection's interface id does not
survive a restart, so a reloaded path is usable again when the peer announces again.

## Interfaces

| Interface id | What | Who |
|---|---|---|
| `tcpb_<n>` | one per connection on the stunnel listener (`:4242`), reached as `reticulum.meshsat.net:4243` with a bridge client certificate | the kits |
| `tcpp_<n>` | one per connection on the public listener (`HUB_RNS_PUBLIC_TCP_ADDR`, `:4244` in k8s), reached as `rns.meshsat.net:4242` (live since 13 Oct 2026, IPv4; shown to tenants from `HUB_RNS_PUBLIC_ENDPOINT`) | stock clients |
| `mqtt_rns` | the legacy Reticulum MQTT topic `meshsat/reticulum/packet` | bridges over the bus |

Every frame on those goes to the node first. What it declines (MeshSat-private contexts: time
sync, custody, HeMB, route hints) falls through to the legacy `internal/reticulum` handler
unchanged, every accepted announce is mirrored into the legacy router so bridge route tables and
hints keep working, and the legacy relay keeps a `tcp` interface that broadcasts to every bridge on
stunnel as before. Iridium, Globalstar, SMS, Tor and WireGuard are legacy-only: no announce from
this node ever reaches a satellite (MESHSAT-1296). The node's own frames echoed back by the MQTT
broker are swallowed. The public listener may carry an interface access code
(`HUB_RNS_PUBLIC_IFAC_NETNAME` / `NETKEY`); off by default, because a stock client must be able to
connect with no key.

## A tenant's address

An owner creates the address on the Reticulum page (`POST /api/lxmf/identity`). The Hub
generates a Reticulum identity and stores it in the tenant's encrypted provider row `lxmf`
(`private_key`, 64 bytes hex in the layout Reticulum's own identity files use, so
`RNS.Identity.from_bytes` loads it to the same address; `display_name`, `stamp_cost`, `enabled`).
The address is the `lxmf.delivery` destination hash, 32 hex characters, shown with a copy button
and a QR code (`GET /api/lxmf/identity/qr.png`).

The leader registers one `lxmf.delivery` destination per enabled tenant on its node and announces
each every `HUB_LXMF_ANNOUNCE_INTERVAL` (30 min) with the display name and stamp cost, and at once
after a change. A change to the row on any replica reaches the leader through
`integrations.ReloadTopic`; a disabled address leaves the node (`rns.Node.RemoveDestination`).

**Stamps.** `stamp_cost` 0 (Sideband's default) accepts every message; a cost makes a sender do
proof of work per message and the Hub enforces it. Above 16 most clients refuse to send.

## Inbox and outbox

Table `lxmf_messages (tenant_id, id)`. Inbound: a per-tenant rate budget
(`HUB_LXMF_INBOUND_PER_MINUTE`, 60; beyond it messages are dropped and counted), a claim per
message hash, the row in state `received` with source, title, content, fields as JSON, method,
stamp value and whether the signature verified (a first message from a peer the node has not
heard announce arrives unverified, as upstream delivers it; the node asks for the peer's path so
the next one verifies). The inbox is capped at `HUB_LXMF_INBOX_MAX` (10000) rows per tenant, the
oldest pruned first, never a message still going out.

Outbound: `POST /api/lxmf/messages` (operator) writes a `queued` row. The leader's sender claims
it (`lxmf-send:<tenant>:<id>`), moves it to `sending`, sends direct across a paid path and
opportunistic otherwise (or as the request says), and moves it to `delivered` on the peer's proof
or `failed` with the reason within `HUB_LXMF_SEND_TIMEOUT` (3 min). A send interrupted by a
failover is failed on the next pass.

## The propagation node

Every tenant address also runs an LXMF propagation node: the `lxmf.propagation` destination on the
same identity, announced with LXMF 1.2's app data (timebase, limits, `[stamp_cost, flexibility,
peering_cost]`, name). A client that sets it as its propagation node hands it a message for a peer
that is offline, one per transfer, as a link packet or a resource, with a propagation stamp at the
node's cost (`propagation_stamp_cost`, LXMF's default 16, flexibility 3). The node validates the
stamp, keeps the message in `lxmf_held (tenant_id, transient_id)` as the encrypted bytes it cannot
read, and serves it on `/get`: a link request after the client identified, `[None, None]` lists what
is held for the client's delivery address, `[wants, haves, limit_kB]` purges what the client already
has and returns the wanted messages within its limit. A message handed in for one of this Hub's own
tenant addresses is decrypted with that identity and filed straight into the inbox as method
`propagated`. Per tenant `HUB_LXMF_PROPAGATION_MAX_BYTES` (50 MiB) and the upstream 30-day expiry;
the oldest go first. Peering between propagation nodes (`/offer` with a peering key) is not offered
yet: the node answers no access. Verified against Python LXMF 1.2.0 (`TestInteropLXMFPropagationHoldAndSync`):
Sideband-style send through the node, sync, purge on the next sync, and the local-address shortcut.

RNS link requests and responses (`internal/rns/request.go`) were added for this: a request is
msgpack `[requested_at, path_hash, data]` as a REQUEST packet or a resource flagged as a request;
the response `[request_id, data]` as a RESPONSE packet or a resource flagged as a response.

## API

| Route | Role | What |
|---|---|---|
| `GET /api/lxmf/identity` | viewer | the address and the propagation node address, or `exists: false` |
| `POST /api/lxmf/identity` | owner | create (idempotent) |
| `PUT /api/lxmf/identity` | owner | `display_name`, `stamp_cost`, `enabled`, `propagation_enabled`, `propagation_stamp_cost` |
| `GET /api/lxmf/identity/qr.png?size=` | viewer | the address as a QR code |
| `GET /api/lxmf/messages?direction=&state=&limit=&before=` | viewer | inbox and outbox, newest first |
| `GET /api/lxmf/messages/{id}` | viewer | one message |
| `POST /api/lxmf/messages` | operator | `destination`, `content`, `title`, `method` (auto, direct, opportunistic) |
| `GET /api/lxmf/peers` | viewer | LXMF peers the address has seen (filled on the leader) |
| `GET /api/rns/status`, `GET /api/rns/paths` | platform admin | the node on this replica: listeners, connections, paths, links, counters |

## Operating it

- **Which replica runs it:** `kubectl -n meshsat-hub get pods -l meshsat.net/rns-leader=true` and
  `kubectl -n meshsat-hub get endpoints hub-rns`. Exactly one pod, both ports.
- **Is the node up:** the leader's log line `rns: upstream-compatible Reticulum node started`;
  `/api/rns/status` on each pod (`active` true on one); the readiness info probe `rns_node`
  reports a leader whose node is not running.
- **A tenant's address is live:** `lxmf: address registered` in the leader's log,
  `GET /api/lxmf/identity` with `active_on_this_replica` true on the leader.
- **Interop:** `make interop` runs the golden vectors and the Hub's node against Python RNS 1.5.5 /
  LXMF 1.2.0 from a pinned venv (`internal/interop/rnsenv`, `~/.cache/meshsat/rns-venv-1.5.5`); CI
  job `test:interop`.
- **The public endpoint** (`rns.meshsat.net:4242`, live since 13 Oct 2026) is the edge relay's
  `rns_in` frontend to `hub-rns:4244` plus, on the three VPS, `patch-haproxy.py rns` and
  `ufw allow 4242/tcp`, plus the Cloudflare A records (not proxied; IPv4 only, `bind :4242` is
  v4-only on HAProxy 2.8). The VPS and DNS parts live in the NL edge repo. `HUB_RNS_PUBLIC_ENDPOINT`
  is what the Reticulum page tells tenants to point a client at; a self-hosted Hub sets its own.
  Proven on 13 Oct with two stock LXMF 1.2.0 clients over the public path (direct both ways, a
  console reply, propagated hold-and-sync).

Never on this path: a plan gate. An address receives whatever its plan is; the plan gates how many
devices and bridges a tenant may register, nothing else.
