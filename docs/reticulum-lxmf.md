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
| `tcpp_<n>` | one per connection on the public listener (`HUB_RNS_PUBLIC_TCP_ADDR`, `:4244` in k8s), reached as `rns.meshsat.net:4242` once the edge carries it | stock clients |
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

## API

| Route | Role | What |
|---|---|---|
| `GET /api/lxmf/identity` | viewer | the address, or `exists: false` |
| `POST /api/lxmf/identity` | owner | create (idempotent) |
| `PUT /api/lxmf/identity` | owner | `display_name`, `stamp_cost`, `enabled` |
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
- **The public endpoint** (`rns.meshsat.net:4242`) is the edge relay's `rns_in` frontend to
  `hub-rns:4244` plus, on the three VPS, `patch-haproxy.py rns` and `ufw allow 4242/tcp`, plus
  the DNS record. The VPS and DNS parts are an infrastructure change, not this repo's.

Never on this path: a plan gate. An address receives whatever its plan is; the plan gates how many
devices and bridges a tenant may register, nothing else.
