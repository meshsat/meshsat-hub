# k8s/NOTES.md — bootstrap state and manual steps (MESHSAT-930)

Things Argo CD cannot do by itself, in the order they happen. Keep this current.

## Done before the tree existed (phase 0, 2026-09-08)

- NL/GR/NO infra: cert-manager `pushsecret_wildcard_meshsat_net` → `k8s/shared/wildcard-meshsat-net-tls`;
  ingress-nginx `proxy-real-ip-cidr` includes TX + relay node IPs; mirror-exempt entries; NO
  `argocd-apps/meshsat-hub/application.yaml` + repo credentials (`gitlab-meshsat-hub-creds`).
- OpenBao seeds under `ci-no/apps/meshsat-hub/`: `hub` (live compose secrets + `HUB_JWT_SIGNING_KEY`,
  `HUB_METRICS_TOKEN`, placeholder `HUB_OIDC_CLIENT_ID/SECRET` = `pending-authentik-bootstrap`),
  `nats` (`NATS_MQTT_PASSWORD`, `bridge_ca_crt`), `redis`, `cnpg` (`meshsat_password`,
  `barman_access_key/secret_key`), `registry` (`ghcr_user`, `ghcr_auth`).
- SeaweedFS bucket `cnpg-meshsat-hub` + identity `cnpg-meshsat-hub`.
- Cloudflare `auth.meshsat.net` (A x3 VPS + AAAA).
- GitLab project 35: deploy key `k8s-pin-bump` (write) + file variable `K8S_PIN_SSH_KEY`;
  `DMZ_DEPLOY_ENABLED=true`.

## Manual, hand-applied cluster state (drift by design, mirror omoikane's)

- **CoreDNS hosts block** for `meshsat.net`: `hub.meshsat.net auth.meshsat.net → 10.255.11.65`
  (ingress VIP), added the same way as the `omoikane.coach` block. Needed for the in-cluster
  OIDC hairpin (Hub → `auth.meshsat.net` discovery/token). Not in git; re-apply after any
  CoreDNS ConfigMap reset. Status: **TODO at phase 1 apply.**
- `hub` Deployment `replicas: 1` since the cutover (2026-09-08); it was 0 until phase 4 passed.

## First sync checklist (phase 1)

1. Argo app `meshsat-hub` Synced; every ExternalSecret `Ready` (`kubectl -n meshsat-hub get es`).
   `hub-secrets` needs all `hub` keys present (placeholders above).
2. CNPG `meshsat-hub-main` 3/3 on dmz03/04/05: `kubectl -n meshsat-hub-db get cluster`.
3. First `ScheduledBackup` landed: `kubectl -n meshsat-hub-db get backups.postgresql.cnpg.io`.
4. **Restore drill**: apply a throwaway `Cluster meshsat-hub-drill` with
   `bootstrap.recovery.source` = `meshsat-hub-main` via `externalClusters` +
   `barmanObjectStore` (same bucket, `serverName: meshsat-hub-main`), compare
   `select count(*)` per table with live, then delete it.
5. `kubectl get pv -o json | jq '.items[].spec.nodeAffinity'` shows no Hub PV on dmz06.
6. From a VPS: `openssl s_client -connect 10.255.4.11:9443 -servername mqtt-hub.meshsat.net`
   asks for a client certificate; `:4243` likewise.
7. `package:stunnel` has pushed `ghcr.io/meshsat/meshsat-hub-stunnel:3.21`; replace the tag pin
   in `kustomization.yaml` with the digest after the first pull.
8. TAK: OpenTAKServer stays on the DMZ hosts. Verify `192.168.192.10:8088/8880` is reachable
   from a Hub pod during rehearsal; if not, route it (edge/xfrm) or set `HUB_TAK_ENABLED=false`
   at cutover and file the follow-up.

## Phase 3: authentik (k8s/scripts/authentik/)

- `run-bootstrap.sh bootstrap` after the Ingress `auth.meshsat.net` serves (needs the tree
  synced and the VPS `meshsat_auth` backend). Then `approve`/`reject` per request.
- n8n workflow `NL - MeshSat Hub Signup Notifier` (id NeiBiyL7igB05ICM, webhook
  `/webhook/meshsat-signup`) is live; Matrix room `#meshsat`, YouTrack project MESHSAT.
- NO CoreDNS also carries `hosts` for `gitlab.nuclearlighters.net` and
  `n8n.nuclearlighters.net` → 192.168.181.43 in the `nuclearlighters.net` zone (2026-09-08):
  the NL forward zone publishes two A records and 192.168.2.43 is unreachable from NO, which
  made the Argo repo-server time out on every clone (omoikane's too, 58 times in 6h).

## Phase 3 applied 2026-09-08

- Edge `patch-haproxy.py auth` applied on NO, CH, TX (backups `haproxy.cfg.bak-20260908-MESHSAT-944*`);
  `meshsat_auth` UP x3 on each. Health check is a static asset because authentik's
  `/-/health/live/` answers 500 on ~50% of requests behind ingress (MESHSAT-968).
- TX had NO established IPsec SAs to the notrf01 DMZ hosts although `no-dmz01..06` are
  configured with `start_action = start` (L4CON on every cluster node); `swanctl --initiate
  --child no-dmz0N` brought all six up. Watch after a TX reboot.
- `run-bootstrap.sh bootstrap` ran: groups, scope mapping, provider + application,
  meshsat-enrollment and meshsat-authentication flows, Brand meshsat.net, notification rule.
  OpenBao `hub` now holds the real HUB_OIDC_CLIENT_ID/SECRET; hub-secrets refreshed.

## Certificates

- `meshsat-net-tls` renews end to end (Let's Encrypt → NL cert-manager → OpenBao → ES).
  NATS reloads via the config-reloader sidecar; **stunnel needs
  `kubectl -n meshsat-hub rollout restart deploy/stunnel`** after a renewal (add a cron or
  Reloader annotation later).
- Bridge CA: Secret `meshsat-bridge-ca`, seeded once by ESO, kept current by the Hub
  (`HUB_BRIDGE_CA_SECRET_NAME`; readiness info probe `bridge_ca_export`).

## NATS bridge users (MR 21, 2026-09-08)

`meshsat-nats-auth` (Secret, key `users.conf`) holds the `authorization` block that
`nats.conf` includes. Git ships only the bootstrap content (shared `meshsat` user, password
resolved from the pod env); the Hub re-renders it on start, every 5 minutes and after every
credential change (`internal/bridge/natsauth.go`: one NATS user per bridge with its bcrypt
hash and permissions confined to its subtree and its tenant's device topics), the reloader
SIGHUPs nats-server, and the Argo Application ignores `/data` on this Secret
(`argocd-apps/meshsat-hub/application.yaml`). Probe: `nats_auth_export` in `/readyz?verbose=1`.
Provisioning bundles now carry the per-bridge user and one-time password; bridges provisioned
before still connect as the shared user until they re-provision.
**Every user in users.conf must carry a `permissions` block** (the renderer gives the shared
user allow-all): nats-server 2.11.8 panics on an authorization reload when a user has none
(`generatePubPerms(nil)` in `mqttCheckPubRetainedPerms`, MESHSAT-973). Re-check when the
`docker.io/library/nats` pin is bumped.

## NATS JetStream x3 (MESHSAT-711, 2026-09-08)

`nats` is a three-pod StatefulSet forming JetStream cluster `meshsat`, **one member per
control-plane node** beside CNPG (`nodeSelector` + control-plane toleration, **required**
hostname anti-affinity, `podManagementPolicy: Parallel`). It is not on the workers: they are 49,
90 and 92 percent committed by other namespaces, and a local volume binds permanently on first
schedule, so `preferred` anti-affinity would let one machine hold two members and take the Raft
group below quorum when it dies.

Services: `nats-headless` (clusterIP None, `publishNotReadyAddresses`) is the governing Service
so `nats-N.nats-headless` resolves for the routes before the pods are Ready; `nats` and `nats-ws`
stay ClusterIP, so the Hub's `tcp://...@nats:1883` and the edge relay's `nats-ws:9443` are
unchanged. `server_name` is the pod name via the downward API.

**Probes differ on purpose.** Readiness is `/healthz?js-server-only=true`: JetStream up *and*
this server current with the meta leader, so a member that cannot see the group is not
endpointed. Liveness is the weaker `/healthz?js-enabled-only=true`: a liveness probe that
depended on cluster state would fail on all three members during a quorum blip, kubelet would
restart all three, and the group could never re-form. Bare `/healthz` additionally sweeps every
stream and consumer, which is too strict for either.

Consequence to know: losing two of three empties the `nats` Service entirely rather than serving
from a member with no quorum. That is deliberate, and it means a two-node loss is a full MQTT
outage, not a degraded one.

PDB is `minAvailable: 2`, the one budget in this tree where 1 would be wrong.

**Cutover, one time (done 2026-09-08).** `serviceName` is immutable *and* the pods moved tier,
so the old 2Gi volume pinned to dmz01 could not follow them. `--cascade=orphan` is the wrong
tool here: the adopted pod would be recreated and stay Pending on a volume it cannot reach, and
in the meantime the old standalone broker and the new members would both be endpoints of the
same Service, which is two brokers serving the same clients. The sequence used instead, with
Argo automation paused:

    kubectl -n meshsat-hub scale sts nats --replicas=0
    kubectl -n meshsat-hub delete sts nats
    kubectl -n meshsat-hub delete pvc data-nats-0        # PV is Retain, bytes stay on dmz01
    argocd app sync meshsat-hub                          # three fresh pods form the cluster

**Streams.** `mqtt.stream_replicas: 3` only governs streams at creation, so the five `$MQTT_*`
streams are recreated at R3 by the adapter on the first client connect. Check with
`/jsz?streams=1` that each has three replicas; a stream created before quorum would silently
stay R1. Of the retained messages only the bond-group config (`/api/bridges/{id}/bond-groups`)
does not regenerate itself: mptcp status re-publishes every 30 s, reticulum route hints every
60 s, credits hourly, and bridge birth messages arrive on reconnect. Restore bond groups by
re-saving them through the API, never by hand-publishing to MQTT.

## KeyDB (MESHSAT-711, 2026-09-08)

The dedup and rate-limit store is `keydb`, a two-pod StatefulSet in multi-master
(`--active-replica yes --multi-master yes`, each pod replicating from the other through the
headless `keydb-hs`), on the control-plane tier. Both accept writes, so losing one machine costs
nothing and there is no failover to wait for.

**The client Service is still called `redis`.** That is deliberate: `HUB_REDIS_URL` and every
other reference kept working, so replacing the store needed no Hub change at all. The workload
is `keydb`, the Service is `redis`, and that mismatch is the one exception to the naming rule.

The old `redis` StatefulSet and its claim were deleted by hand after the rollout, because this
Argo application does not prune. Its volume is Retain, so `pvc-221b3d89...` on dmz01 still holds
the bytes and is the way back.

Two things to know: dedup is no longer linearizable, because two masters behind one Service can
both accept the same key inside the replication window. Replication is sub-millisecond on this
LAN, the consumer already fails open, and `dispatch_claims` in Postgres is what actually
guarantees single delivery. And KeyDB upstream is quiet since the acquisition; the mitigation is
that this exact image digest has been in production in the omoikane namespace since August.

## The deep basemap (MESHSAT-967, 2026-09-08)

**The build needs memory, and Go will not ask for it politely.** The first
attempt ran the extractor with a 1Gi limit and it was OOMKilled 34 seconds in,
during chunk fetching rather than the directory build. The extractor is Go: left
alone the runtime grows the heap until the cgroup kills the process, and a
killed build has already pulled several GB for nothing. The fix is `GOMEMLIMIT`
at 3000MiB under a 4Gi limit, so the collector runs instead of the OOM killer.
If you ever widen the bbox or raise the zoom, raise both together.

The map reads two archives. The **world** one lives in the object store and the Hub streams it
at `/basemap/basemap.pmtiles`; it stops at zoom 11, which is as deep as a global archive can be
and still fit there. The **deep** one is Europe to zoom 15, which is the zoom where street names
exist, and it is 37 GB.

That 37 GB is why it is not in the object store: the SeaweedFS volume servers sit on dmz01 and
dmz06 with about 35 GB free between what each can spare, and filling those has taken the S3
write path down before. The control-plane machines have 121 to 129 GB free each, so the archive
sits on their own disks: StatefulSet `basemap`, two replicas, one per machine, 60 Gi claims so
it cannot creep, `Retain` on delete. nginx serves the file and the ingress sends
`/basemap/local.pmtiles` straight there, so the Hub is not in the path and stays stateless.

**It builds itself.** The init container extracts the archive from the Protomaps daily planet
build straight onto the volume when the volume does not have it: a new machine, a restored
volume, or a deliberate rebuild after `rm`. The extractor is pinned by version and verified by
checksum, and cached at `/data/bin/pmtiles`, so a restart costs nothing. The first build pulls
about 37 GB and takes tens of minutes; the two replicas do it one at a time.

Two things that will eventually bite:

- Planet builds are kept for roughly a week. If a volume is empty and `PLANET_URL` points at an
  expired build, the extract fails, the pod stays unready, and the map quietly falls back to the
  world archive. Update the date in `basemap-config` and it rebuilds.
- Coverage is a bounding box in the same ConfigMap. Widening it means more gigabytes: measured
  against the 2026-09-07 build, Germany alone is 6.8 GB at zoom 15, the Netherlands and Greece
  together 2.6 GB, all of Europe 37 GB, and the entire planet 128 GB.
