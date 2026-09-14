# Tor hidden service

Deployed for MESHSAT-1121. `docker-compose.yml` has had a `tor` service since the
compose era and it never got a k8s manifest, so the Hub's `/api/tor/onion` has
reported "unavailable" since the cutover.

## Why the .onion reaches the Hub as an env var, not a file

`internal/tor.NewService` reads a hostname FILE, which suited compose where both
containers shared a volume. Here Tor and the Hub are different pods and the
volume is RWO node-local, so the Hub cannot mount it. The address is therefore
published as `HUB_TOR_ONION` in the Hub's ConfigMap, and `NewService` prefers
that over the file.

That is not a workaround: a .onion is DERIVED FROM THE KEY and is stable for the
life of the volume, so it is configuration, not state to be discovered at runtime.

## Bootstrapping the address — DONE, and how to redo it

The address is not known until Tor has generated a key once, so this was a
one-time follow-up and it has been done: `HUB_TOR_ONION` is set in
`k8s/hub/configmap.yaml`.

If the key volume is ever lost or deliberately rotated, the address CHANGES and
every published reference to it breaks. To re-derive it:

1. Tor starts and writes `/var/lib/tor/hidden_service/hostname`.
2. `kubectl -n meshsat-hub exec tor-0 -- cat /var/lib/tor/hidden_service/hostname`
3. Put that value in `HUB_TOR_ONION` in `k8s/hub/configmap.yaml` and merge.

That is why the storage class is `retain` and why this key is the one thing here
worth backing up.

## Bootstrapping takes minutes, and that is normal

Tor loads a consensus and relay descriptors before the service is reachable
("Bootstrapped 55% (loading_descriptors)"). The hostname file exists long before
the service answers, so the presence of an address is not evidence that anything
can connect to it yet.

## Single replica, deliberately

The .onion is the public half of the key on that volume. Two replicas would mean
either two different addresses or a shared private key, and neither is what
anybody means by "the Hub's onion address".
