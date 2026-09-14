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

## Bootstrapping the address

It is not known until Tor has generated a key once:

1. Merge this directory; Tor starts and writes `/var/lib/tor/hidden_service/hostname`.
2. `kubectl -n meshsat-hub exec deploy/tor -- cat /var/lib/tor/hidden_service/hostname`
3. Put that value in `HUB_TOR_ONION` in `k8s/hub/configmap.yaml` and merge.

Steps 2-3 are one-time. If the volume is ever lost the address changes and every
published reference to it breaks, which is why the class is `retain` and why the
key is worth backing up.

## Single replica, deliberately

The .onion is the public half of the key on that volume. Two replicas would mean
either two different addresses or a shared private key, and neither is what
anybody means by "the Hub's onion address".
