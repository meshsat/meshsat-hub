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

## NATS JetStream x3 (MESHSAT-711 phase 7, 2026-09-08)

`nats` is a three-pod StatefulSet (`podManagementPolicy: Parallel`, spread over dmz01/dmz02 with
preferred anti-affinity since only two workers are eligible, never dmz06) forming JetStream
cluster `meshsat` over routes on :6222; losing the node that holds two pods loses quorum until it
returns (today a single node holds everything); `server_name` is the pod name
(downward API). `nats-headless` (clusterIP None, `publishNotReadyAddresses`) is the governing
Service so `nats-N.nats-headless` resolves for the routes before the pods are Ready; `nats`
(ClusterIP, Ready endpoints only) stays the Hub's broker address and `nats-ws` the edge relay
target. Readiness is `/healthz?js-enabled-only=true`: a server without a JetStream meta leader
is not endpointed. **One-time hand step at rollout** (serviceName is immutable): with Argo
automation paused, `kubectl -n meshsat-hub delete sts nats --cascade=orphan`, then sync; the
new StatefulSet adopts nats-0 and rolls it onto the new template, nats-1/nats-2 start in
parallel. `mqtt.stream_replicas: 3` makes new
MQTT session/retained/QoS streams R3; streams created on the single node stay R1 until edited
(`nats stream edit --replicas 3` from a `natsio/nats-box` pod, or delete them while no bridge is
connected: NATS recreates them). Rolling the StatefulSet restarts the broker: the Hub and the
bridges reconnect, in-flight QoS 1 publishes are retried by the clients.
