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

That is why the storage class is `retain` -- but the volume is no longer the only
copy. See below.

## The key is backed up, and restores itself

The identity key lives in OpenBao at `ci-no/apps/meshsat-hub/tor`
(`HS_ED25519_SECRET_KEY_B64`, the public key, and the hostname). `externalsecret.yaml`
pulls it into the Secret `tor-identity`, and the `seed-identity` initContainer
writes it onto the volume **only when the volume has no key of its own**.

So a lost volume, a rebuilt cluster or a replaced node all come back with the
SAME .onion instead of a new one that breaks every published reference.

Two deliberate choices in that initContainer:

- **It never overwrites.** While the volume has a key, the volume is
  authoritative. Clobbering a live key on every restart would be a way to LOSE
  one rather than protect it.
- **Permissions are asserted on every start, not only on a restore.** tor
  refuses to start on a key it considers too readable, so `seed-identity` sets a
  0700 directory, 0600 files and uid 100 (`tor` in this image) each time.
  Getting the bytes right and the mode wrong fails just as completely.

## Do not add `fsGroup`, however obviously right it looks

`fsGroup: 100` is the textbook way to hand a volume to a non-root uid, it was
here, and it is what broke Tor. The kubelet's default `fsGroupChangePolicy` is
`Always`: on **every** pod start it walks the volume, chgrps to the fsGroup and
ORs in group bits, so a 0700 directory becomes `drwxrws---` (2770) and a 0600
key becomes 0660. tor's `check_private_dir` then refuses the config outright:

```
[warn] Permissions on directory /var/lib/tor/hidden_service are too permissive.
[err]  Reading config failed--see warnings above.
```

It hid for as long as the pod was never restarted, because tor creates the
directory 0700 itself *after* the kubelet has finished with the volume. The
first restart after that is when it dies — so it was latent from the first
deploy and went off on an unrelated commit, which is the worst way to find it.

`seed-identity` runs as root and sets ownership and modes directly, which is
deterministic and needs nothing from the kubelet.

To rotate to a RANDOM new address: delete the PVC *and* the OpenBao entry, let
Tor generate a fresh key, then store the new one the same way.

To install a KNOWN key instead -- a vanity address, or a rollback -- do NOT
delete the OpenBao entry, because the restore path is what installs it. The
order is load-bearing (done for real in MESHSAT-1193):

1. Back the current identity up to another OpenBao path first. It is the only
   copy that is not on one node's disk.
2. Write the new `HS_ED25519_*_B64` and `HOSTNAME` into
   `ci-no/apps/meshsat-hub/tor`, carrying `ONION_HEARTBEAT_SECRET` across
   unchanged -- that is the status-page credential, not part of the identity,
   and replacing it silently breaks the monitor.
3. Force the ExternalSecret to resync (`kubectl annotate es tor-identity
   force-sync=$(date +%s) --overwrite`) and CONFIRM the Secret carries the new
   hostname. The refreshInterval is 1h, so without this the next step restores
   the OLD key and the change looks like it worked.
4. Only now clear the key files on the volume and delete `tor-0`, so
   `seed-identity` takes the restore branch.
5. Update `HUB_TOR_ONION` and merge.
6. Verify against the NEW address, not the config value: `onion-heartbeat`
   fetches whatever `tor-identity/hostname` says, so a green heartbeat after
   the roll is proof the new identity is actually serving.

The trap worth naming: updating OpenBao and the ConfigMap alone changes nothing,
because `seed-identity` never overwrites a key that is already on the volume.
Every symptom says success and the old address keeps answering.

## Bootstrapping takes minutes, and that is normal

Tor loads a consensus and relay descriptors before the service is reachable
("Bootstrapped 55% (loading_descriptors)"). The hostname file exists long before
the service answers, so the presence of an address is not evidence that anything
can connect to it yet.

## Single replica, deliberately

The .onion is the public half of the key on that volume. Two replicas would mean
either two different addresses or a shared private key, and neither is what
anybody means by "the Hub's onion address".
