# wg-easy — the platform's own WireGuard server

Deployed for MESHSAT-1121. Until then `HUB_WG_ENABLED` was `false` and no wg-easy
existed anywhere in the estate, so "Network" was a nav entry in front of nothing.

**This is the PLATFORM's server, serving the default tenant only.** Every other
tenant brings its own on the Integrations page; the Hub creates peers on whichever
server the tenant configured, and a tenant with none simply has no VPN. See
`internal/wireguard/pool.go`.

## Why the control-plane tier

Measured 2026-09-14: the worker tier (dmz01/02/06) was at 90% memory on two of
three nodes; the control-plane tier (dmz03/04/05) at 6-7%. wg-easy is ~100Mi, so
capacity was never the question — the placement follows the database, NATS and
KeyDB, which already live there.

## The privilege, stated rather than buried

wg-easy needs `NET_ADMIN` and the `wireguard` kernel module to bring up an
interface, and nothing else in this namespace is privileged. That is a real
widening of what a compromise of this namespace could do, and it is the reason
this file exists rather than the capability being added quietly in a patch.

It does NOT need `hostNetwork`: the UDP listener is a Service. It runs as a
single replica because a WireGuard interface and its peer database are not
something two pods can share — the peer list is wg-easy's own state on disk.

## Peer state

`/etc/wireguard` holds the server key and the peer database on a node-local
volume (openebs local-hostpath; there is no CSI in this cluster). Losing it
changes the server's public key and invalidates every peer configuration already
handed out, so it is the one thing here worth backing up.
