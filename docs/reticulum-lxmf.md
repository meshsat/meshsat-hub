# The Hub as a Reticulum node: tenant LXMF addresses, inbox and outbox

Since MESHSAT-1555 the Hub runs an upstream-compatible Reticulum node and gives every tenant an
LXMF address on it. A Sideband, CrossTalk, MeshChat or NomadNet user messages that address; the
message lands in the tenant's inbox in the console and the API, and what the tenant sends is
marked delivered when the peer proves it. The implementation is the Bridge's, verified against
Python RNS 1.5.7 and LXMF 1.2.1 (1.5.5 / 1.2.0 until 27 Oct 2026; `internal/rnswire`, `internal/rns`, `internal/msgpack`,
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

Every frame on those goes to the node first. What it declines from `tcpb_*` or `mqtt_rns`
(MeshSat-private contexts: time sync, custody, HeMB, route hints) falls through to the legacy
`internal/reticulum` handler unchanged, every announce accepted there is mirrored into the legacy
router so bridge route tables and hints keep working, and the legacy relay keeps a `tcp` interface
that broadcasts to every bridge on stunnel as before. **Nothing from `tcpp_*` reaches the legacy
layer** (MESHSAT-1720): what the node declines there is dropped and counted (`public_dropped` in
`GET /api/rns/status`), and a public announce teaches the node a path but is never mirrored,
because the legacy layer trusts its `tcp` interface the way it trusted certificate-holding kits. Iridium, Globalstar, SMS, Tor and WireGuard are legacy-only: no announce from
this node ever reaches a satellite (MESHSAT-1296). The node's own frames echoed back by the MQTT
broker are swallowed. A platform-wide interface access code (`HUB_RNS_PUBLIC_IFAC_NETNAME` /
`NETKEY`, unset in production) would make the main node admit only clients with that code; the
tenants' own codes are per tenant, below.

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
A node announces nothing to a client that connects, so a client that had not asked for a path
did not list the address for up to 30 minutes (MeshChat showed "Unknown Peer", MESHSAT-1561).
Since then a connection joining a node (`rnsnode.Config.OnJoin`: the main node, or a Private
tenant's once its code matched) has the addresses on that node announced again, at most once a
minute each; a join inside the minute is announced when the minute is up, so every joiner hears
the address within a minute. A stranger joining the main node hears only Public addresses, and
connecting repeatedly costs at most one announce per Public address a minute.

**Stamps.** `stamp_cost` 0 (Sideband's default) accepts every message; a cost makes a sender do
proof of work per message and the Hub enforces it. Above 16 most clients refuse to send.

## Who can reach an address (MESHSAT-1722)

Each tenant chooses, on the Reticulum page (owner), its `access_mode` in the provider row:

| Mode | Where the addresses live | Who reaches them |
|---|---|---|
| **private** (default; also an empty or unknown value) | the tenant's OWN node (`rnsnode.Service.PrivateNode`) | only a public connection that presented the tenant's network name and passphrase (`ifac_netname`, `ifac_passphrase`, generated by the Hub, 32 random bytes; shown to owners and operators) |
| **public** | the main node | anyone on Reticulum, like an email address |
| **off** | nowhere | nobody |

**One port, one node per Private tenant.** A `tcpp_*` connection starts unclassified and is in no
node's interface list. Its first frame with an interface access code is tried against each
Private tenant's key (bounded by `HUB_RNS_IFAC_TRIES_PER_SECOND`, 200, across the listener: the edge
hides client addresses); a match binds the connection to that tenant's node for its lifetime. A
frame without a code binds it to the main node. Three frames whose code matches nothing close it;
a connection nothing classified is closed after `HUB_RNS_UNCLASSIFIED_TIMEOUT` (60 s). A Private
tenant's node is a separate `rns.Node` on the Hub's transport identity with its own path, announce,
link and dedup tables, whose transmitter sees only its bound connections; the main node's
transmitter skips them. So a case nobody thought of fails closed. Phase 1 changed nothing in `rns`,
`rnswire` or `lxmf`. A Private node's paths are in memory only.

**Moving between modes.** A mode change reaches the leader through `integrations.ReloadTopic`:
the address is unregistered from its node and registered on the new one, the tenant's own node is
stopped when it leaves Private, and the main node forgets any cached path to an address that is not
on it (`ForgetPaths`, memory and `rns_paths`). Replacing the passphrase (`POST
/api/lxmf/access/rotate`) closes every connection bound with the old one at once. An address made
before 27 Oct 2026 had no mode: it is Private, and the leader gave it a code on first registration
(audit `lxmf_access_code_created`).

**What Private does not promise** (also on the page): a destination hash is not a secret, and a
member's own app announces on every interface it has; anyone with the passphrase can let others in
(a member running a transport node re-floods the tenant's announces); Private is a closed island
(no public destinations, no propagation peering out of it); the Hub's transport id is the same in
every class; the passphrase is one shared secret per tenant, so removing one person means rotating
it for everyone. Off stops the Hub serving and remembering an address; a client that cached it while
it was Public keeps it until its own cache expires.

**A kit joins a Private network as a leaf (phase 2 v1, 27 Oct 2026).** The kits' own entrance
(stunnel, `tcpb_*`) carries no tenant, so a kit joins the way an app does: a second Reticulum
connection to `rns.meshsat.net:4242` with the tenant's network name and passphrase. On the Bridge
that is a `tcp_rns` row with `ifac_netname` and `ifac_passphrase` (`POST /api/routing/ifaces` on
the kit; Settings has no form for it). A coded row carries only the kit's own traffic: the kit's
node marks it a leaf (`rns.Node.SetLeaf`, in the shared package of both repos, unused by the Hub),
so announces, path requests and packets are never relayed between the tenant's network and the
kit's LoRa mesh, the Hub's main node over MQTT, or any other link; the row is kept out of the
Bridge's interface registry and processor, so the legacy layer cannot use it; and the Bridge sends
nothing on it that the node did not wrap in the code; a path learned on the row never displaces a
live path the kit has through another interface. LXMF to and from the kit's own address is the
kit's own traffic: the workspace's access rules on the kit (`lxmf_0` to a satellite bearer and back,
MESHSAT-1554) are the one deliberate way across, named senders only. The design attack found that without the leaf
a joined kit, being a transport node, bridges the tenant's network to its mesh; the Bridge's interop
test reproduces exactly that against stock RNS 1.5.7 when the leaf mark is removed
(`TestInteropCodedRowIsALeafOfThePrivateNetwork`). On the Hub nothing changed: the kit's connection
is classified like any app's.

**The Hub hands each kit its code (MESHSAT-1715, 27 Oct 2026).** A kit asks on its own
`rns/join` and the leader answers on its `rns/access` with the stored network name and passphrase,
or `none` for a Public, Off or missing address; a mode change or a rotation is pushed to every
bridge of the tenant at once. The kit keeps one `tcp_rns` row marked `"managed":"hub"` to match,
with the host and port of its own setting (`MESHSAT_HUB_RNS_ENDPOINT`, default
`rns.meshsat.net:4242`) and only the code from the message, so a rotation reaches the kits within
seconds and nobody types anything. The broker refuses `rns/access` (and every other Hub-only topic)
from every bridge of the tenant, not just for the bridge's own id. Contract, limits and the kit's
asking: `docs/bridge-hub-protocol.md` "Reticulum access". The kits' inbound LXMF is capped at 60
messages a minute (`MESHSAT_LXMF_INBOUND_PER_MINUTE`). A deleted or lost kit keeps the code it was
told: rotate it. Deleting a kit while the workspace has a code records `lxmf_access_rotation_advised`
in the audit log, and the Kits and Reticulum pages offer the rotation until the network name changes
(`rotation_advised` on the identity, MESHSAT-1731). Both kits joined the platform workspace's network on 27 Oct 2026 (display names
"MeshSat Kit 1" and "MeshSat Kit 2", without the hostnames).

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

## Voice notes and photos (MESHSAT-1735)

A message's `FIELD_AUDIO` (`[mode, bytes]`: Codec2 0x01-0x09, 700C = 3; Opus 0x10-0x19) and
`FIELD_IMAGE` (`["webp", bytes]`), the shapes Sideband and MeshChat send, are kept beside the
row: the metadata in `lxmf_attachments (tenant_id, message_id, idx)`, the bytes in the audit
archive's object store (`HUB_AUDIT_ARCHIVE_S3_*`) under `HUB_LXMF_ATTACHMENT_S3_PREFIX`
(`lxmf-attachments`) `/<tenant>/<message id>/<index>`; `fields_json` leaves them out. One
audio and one image per message, together at most 64 KiB (a received message over it keeps its
text only); a tenant keeps at most `HUB_LXMF_ATTACHMENT_MAX_BYTES` (64 MiB), the oldest evicted
first, bytes before row. Without an object store the text is kept, the attachments are not, and
a send with one is refused. `POST /api/lxmf/messages` takes `audio: {mode, data_b64}` and
`image: {format, data_b64}` (content may be empty then); the bytes are stored before the row is
queued, so the leader, on either replica, finds them. The same file (`internal/lxmf/fields.go`)
reads and writes them on the kits; the Bridge's interop test proves the wire against Python
LXMF 1.2.1.

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

**The apps, 27 Oct 2026 (MESHSAT-1561), against production, each with a new identity on the
platform workspace's Private network.** MeshChat 2.4.0 (bundles RNS 1.3.7, LXMF 1.0.1) and
CrossTalk 2.6.0 (RNS 1.5.5, LXMF 1.2.0) ran their own Python back ends headless on Linux and were
driven through their own web screens in a browser; NomadNet 1.4.4 (RNS 1.5.7, LXMF 1.2.1) ran its
text UI in a pseudo-terminal. Each took the network name and passphrase in its own place
(MeshChat and CrossTalk: Interfaces, Add Interface, TCP Client, IFAC Settings; MeshChat asks for
a restart, CrossTalk applies it at once; NomadNet: the Reticulum config file). In each, a message
to the workspace address sent over a direct link showed delivered with the name "MeshSat"
(MeshChat and CrossTalk in 4.7-4.8 s), the console's reply from the Reticulum page arrived, and
with the workspace's propagation node set in the app's settings a message held for it arrived on
the app's own Sync (MeshChat and CrossTalk 4.6 s; NomadNet "Downloaded 1 new messages"). The
apps also heard each other and both kits by name through the network. Two differences from the
Python probe: NomadNet's New Conversation stores the name typed there, so a blank name shows no
name in the conversation (the announce stream shows "MeshSat"); and an app that joined after the
address's last announce did not list it until it asked for a path, which the announce on join now
covers. Not covered: Sideband's screens on a phone, and the desktop apps' Electron windows (the
same screens, served by the same back end).

RNS link requests and responses (`internal/rns/request.go`) were added for this: a request is
msgpack `[requested_at, path_hash, data]` as a REQUEST packet or a resource flagged as a request;
the response `[request_id, data]` as a RESPONSE packet or a resource flagged as a response.

## API

| Route | Role | What |
|---|---|---|
| `GET /api/lxmf/identity` | viewer | the address, the propagation node address, `access_mode` and `ifac_netname`, and `ifac_passphrase` for operators and owners (`ifac_passphrase_hidden` for a viewer), `rotation_advised` (kits deleted under the current network name), or `exists: false` |
| `POST /api/lxmf/identity` | owner | create (idempotent) |
| `PUT /api/lxmf/identity` | owner | `display_name`, `stamp_cost`, `enabled`, `propagation_enabled`, `propagation_stamp_cost`, `access_mode` (private, public, off) |
| `POST /api/lxmf/access/rotate` | owner | a new network name and passphrase for the Private mode; the old ones' connections are closed |
| `GET /api/lxmf/identity/qr.png?size=` | viewer | the address as a QR code |
| `GET /api/lxmf/messages?direction=&state=&limit=&before=` | viewer | inbox and outbox, newest first |
| `GET /api/lxmf/messages/{id}` | viewer | one message |
| `POST /api/lxmf/messages` | operator | `destination`, `content`, `title`, `method` (auto, direct, opportunistic), `audio` `{mode, data_b64}`, `image` `{format, data_b64}` (together at most 64 KiB; 413 over it) |
| `GET /api/lxmf/messages/{id}/attachments` | viewer | the message's voice note and photo: `kind`, `mode` or `format`, `size`, `content_type`, `url` |
| `GET /api/lxmf/messages/{id}/attachments/{n}` | viewer | the bytes: `audio/codec2` with `X-LXMF-Audio-Mode`, `audio/ogg`, `image/webp` |
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
- **Interop:** `make interop` runs the golden vectors and the Hub's node against Python RNS 1.5.7 /
  LXMF 1.2.1 from a pinned venv (`internal/interop/rnsenv`, `~/.cache/meshsat/rns-venv-1.5.7`); CI
  job `test:interop`. Test binaries share the venv under a lock file beside it (`<venv>.lock`,
  MESHSAT-1725), so a fresh cache is built once.
- **The public endpoint** (`rns.meshsat.net:4242`, live since 13 Oct 2026) is the edge relay's
  `rns_in` frontend to `hub-rns:4244` plus, on the three VPS, `patch-haproxy.py rns` and
  `ufw allow 4242/tcp`, plus the Cloudflare A records (not proxied; IPv4 only, `bind :4242` is
  v4-only on HAProxy 2.8). The VPS and DNS parts live in the NL edge repo. `HUB_RNS_PUBLIC_ENDPOINT`
  is what the Reticulum page tells tenants to point a client at; a self-hosted Hub sets its own.
  Proven on 13 Oct with two stock LXMF 1.2.0 clients over the public path (direct both ways, a
  console reply, propagated hold-and-sync).

Never on this path: a plan gate. An address receives whatever its plan is; the plan gates how many
devices and bridges a tenant may register, nothing else.
