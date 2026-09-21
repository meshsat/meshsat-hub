# MeshSat Hub — Hosted deployment notes

These are the operator's notes for the **hosted** MeshSat Hub at `hub.meshsat.net`. They describe
one cluster, one identity provider and one object store, and they name them.

If you want to run your own Hub, this is not the page: the customer-facing guide is
[docs.meshsat.net/hub/self-hosting](https://docs.meshsat.net/hub/self-hosting). It covers the
single-host Compose stack, the Kubernetes tree, sizing, and every setting an operator owns. The
Hub is Apache 2.0 and a self-hosted one is a first-class deployment.

---

## The hosted cluster

The Hub runs on `notrf01cl01k8s` from the kustomize tree in `k8s/`, synced by the Argo CD
Application `meshsat-hub`. Every merge to main builds an image, the `bump_k8s_pin` CI job
rewrites its digest into `k8s/kustomization.yaml` with `[skip ci]`, and Argo rolls the
Deployment. Argo has `selfHeal` on and **prune off**: a manifest that stops existing is not
deleted from the cluster, so removing a workload is a merge plus a hand `kubectl delete`.

```bash
kubectl kustomize k8s/ | kubeconform -strict -ignore-missing-schemas   # what CI checks
kubectl --context notrf01 -n meshsat-hub get pods
kubectl --context notrf01 -n meshsat-hub logs deploy/hub | sed -n 1p    # the commit it was built from
```

| Resource | Kind | Replicas | Purpose |
|----------|------|----------|---------|
| hub | Deployment | 2 | API + SPA; RollingUpdate surge 1, PDB minAvailable 1, no volume |
| meshsat-hub-main | CNPG Cluster | 3 | Postgres, synchronous replica, daily barman backup to S3 |
| nats | StatefulSet | 3 | MQTT + WebSocket mTLS bus, JetStream |
| redis (KeyDB) | StatefulSet | 2 | Dedup, rate limits, multi-master |
| stunnel | Deployment | 1 | Reticulum TLS termination |
| meshsat-edge-relay | DaemonSet | workers | hostNetwork haproxy :8443/:9443/:4243 for the VPS edge |
| hub-verify | CronJob | nightly | in-cluster verification suites (see below) |
| hub.meshsat.net / auth.meshsat.net | Ingress | -- | ingress-nginx, wildcard cert from OpenBao |

Single-owner work (reapers, retention, the receipt and refund drainers, the OTS poller) runs on
the holder of a Kubernetes Lease; message dispatch and every other side effect are protected by
database claims (`store.ClaimOnce`), not by the leader, so both replicas process every message
and exactly one acts on it.

**Traffic** arrives through three VPS HAProxy edges (Norway, Switzerland, Texas), which target
the three workers' mesh addresses on the edge relay's host ports. Editing the edge is
`k8s/scripts/edge/patch-haproxy.py`, applied by hand with a backup and `haproxy -c` first.

**Identity** is the shared authentik in namespace `omoikane`, Brand meshsat.net, bootstrapped by
`k8s/scripts/authentik/run-bootstrap.sh`. Enrollment is an approval, not a self-service signup.

**Secrets** come from OpenBao through ExternalSecrets (`ci-no/apps/meshsat-hub/*`). Stakater
Reloader watches the Hub's ConfigMap and Secret and rolls the Deployment when either changes;
`RELOADER_CANARY` in `k8s/hub/configmap.yaml` exists only to drill that. A rotated secret needs
no `rollout restart`.

**Storage** is node-local only (openebs local-hostpath). Redundancy comes from each service
replicating itself. The audit archive and the map basemap live in the object store at
`nl-s3.nuclearlighters.net`, bucket `cnpg-meshsat-hub`, beside the database backups.

Conventions in `k8s/CONVENTIONS.md`; bootstrap and hand-applied state in `k8s/NOTES.md`;
operator tooling under `k8s/scripts/`.

## Configuration reference

Configuration is a YAML file (`HUB_CONFIG_FILE`) with `HUB_*` environment overrides. In the
hosted cluster the values are the ConfigMap `hub-config` and the Secret `hub-secrets`. Every
setting that describes a **tenant's** behaviour is in the UI, not here; the environment holds
platform values and the defaults a tenant inherits (`internal/config/classification.go` maps
every field to its owner and the test fails the build on an unclassified one).

| Variable | Description |
|----------|-------------|
| `HUB_MODE` | `kubernetes` here (`standalone` is the default for a single host) |
| `HUB_DB_DRIVER` | `postgres` (sniffed from `HUB_DATABASE_URL` when unset) |
| `HUB_DATABASE_URL` | the CNPG `-rw` service, `sslmode=require` |
| `HUB_REDIS_URL` | KeyDB, Service still named `redis` |
| `HUB_MQTT_BROKER_URL` | the NATS MQTT listener, `tcp://nats:1883` in-cluster |
| `HUB_MQTT_CLIENT_ID` | **must carry the pod name**, or two replicas evict each other at the broker |
| `HUB_OIDC_ISSUER_URL` | the authentik provider; its presence selects `oidc` auth mode |
| `HUB_METRICS_TOKEN` | bearer token guarding `/metrics`; the PodMonitor carries it |
| `HUB_PLAN_<TIER>_DEVICES` | device+bridge ceiling per plan (`free` 4, `crew` 24, `fleet` 100, -1 unlimited) |
| `HUB_SMTP_RELAY`, `HUB_MAIL_FROM` | transactional mail via `nllei01smtp-dkim01:2525`, sent as `billing@meshsat.net` |
| `HUB_STRIPE_*` | three separate secrets: API key, webhook signing secret, path secret |
| `HUB_TOR_ONION` | the published `.onion`, derived from the key in `k8s/tor`'s volume |

The full list, with defaults, is `config.example.yaml`. `internal/config` refuses the literal
string `<no value>`, which External Secrets renders for a key missing from OpenBao.

## Map basemap (self-hosted)

The Hub's map and geofence pages render a vector basemap that the Hub serves
itself, so an operator's browser never asks a third-party tile host for tiles
and the areas they look at, which are roughly where their devices are, stay
inside the deployment.

One PMTiles archive and its glyph and sprite assets live in an S3-compatible
bucket; the Hub streams them at `/basemap/basemap.pmtiles` and
`/basemap/assets/`, forwarding HTTP range requests, so a session transfers the
few tiles it displays rather than the whole archive. The routes serve public
OpenStreetMap-derived data and need no authentication.

The map uses **two** archives, and that split is forced rather than chosen: in this basemap
schema street geometry appears at zoom 13 and street names at zoom 15, and a world archive that
deep does not fit in any object store we have. So the world archive covers the globe shallowly
and a second one covers the area the fleet operates in all the way down. The deeper layers draw
on top from zoom 11; outside their coverage they simply have no tiles and the world shows
through. Sizes measured against the 2026-09-07 planet build: world zoom 0-8 is 0.5 GB and shows
no streets at all, 0-11 is 7.9 GB and shows major roads with their names, the Netherlands at
0-15 is 2.0 GB and shows everything.

| Variable | Description |
|----------|-------------|
| `HUB_BASEMAP_S3_KEY` | Object key of the world PMTiles archive. Empty disables the map backdrop. |
| `HUB_BASEMAP_S3_LOCAL_KEY` | Object key of the deeper regional archive. Empty means world only. |
| `HUB_BASEMAP_S3_ASSET_PREFIX` | Key prefix of the glyphs and sprites (default `basemap/assets`) |
| `HUB_BASEMAP_S3_ENDPOINT` / `_BUCKET` / `_REGION` | Object store; default to the audit archive's values |
| `HUB_BASEMAP_S3_ACCESS_KEY` / `_SECRET_KEY` | Credentials; default to the audit archive's |
| `HUB_BASEMAP_CACHE_MAX_AGE` | `Cache-Control` max-age of the archive (default `24h`) |

Build and publish either archive with `k8s/scripts/basemap/build-basemap.sh`, which extracts
from the Protomaps daily planet build over range requests, uploads it with the font and sprite
assets, and prints the config values to set. Adding another operating area is one command, for
example `build-basemap.sh 20260907 15 19.3,34.8,28.3,41.8 gr` for Greece.
Without the key the map still draws devices, tracks and geofences on an empty
backdrop and says so.

Attribution: map data (c) OpenStreetMap contributors (ODbL), basemap tiles by
Protomaps, label fonts Noto Sans under the SIL Open Font License. The
attribution control on the map carries the first two.

## mTLS bridge authentication

Bridges connect over MQTT-over-WebSocket with mutual TLS. The Hub is the certificate authority
and issues ECDSA P-256 client certificates (90 days) per bridge; NATS terminates TLS and
verifies the certificate against the Hub's bridge CA.

```
Bridge (field device)
  |
  | wss://mqtt-hub.meshsat.net/mqtt  (client cert + key)
  v
VPS HAProxy :443, TCP mode, SNI peek, no TLS termination
  v
edge relay on a worker (hostNetwork :9443)
  v
NATS websocket :9443, tls { verify: true, ca_file: bridge CA }
  | client cert CN = bridge_id, signed by the Hub CA
  v
MQTT session
```

Onboarding is the Kits page: add the kit, then either show the setup QR (the kit or the
Android app scans it) or issue the broker login (shown once) and the certificate (shown once)
and paste URL, credentials and PEM into the bridge's Hub Connection settings. The same three steps exist as `POST /api/bridges/{id}/credentials` and
`/certificate`. The bridge must **not** be given the bridge CA as its root store, or it can no
longer verify the server's Let's Encrypt certificate. The production NATS config is
`k8s/nats/configmap.yaml`; one NATS user per bridge is rendered by the Hub into the Secret
`meshsat-nats-auth`, and every user needs a permissions block or the server panics on reload.

## Backup and restore

The database is backed up by CNPG's barman plugin to `s3://cnpg-meshsat-hub` (daily base backup,
continuous WAL archiving). What is alerted on is the **age** of the last successful backup, not
the success flag of the last attempt; a failed Backup object wedges the slot until it is deleted,
and the hourly CronJob in `k8s/db/backup-unwedge.yaml` does that.

**The restore has been drilled.** `docs/restore-drill.md` is the procedure and its log; the
nightly verification fails when the last recorded drill is older than the policy allows, so the
drill cannot quietly lapse. The tenant-level export (`GET /api/tenant/export`) and the
platform backup endpoints below are application-level and do not replace it.

```bash
curl -o backup.zip https://hub.meshsat.net/api/backup/export -H "Authorization: Bearer $TOKEN"
curl -X POST https://hub.meshsat.net/api/backup/diff   -H "Content-Type: application/zip" --data-binary @backup.zip
curl -X POST https://hub.meshsat.net/api/backup/import -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/zip" --data-binary @backup.zip
```

## What watches it

- Alert rules live in the NL infrastructure repo (`k8s/namespaces/monitoring/meshsat-hub-alerts.tf`):
  per-edge probes, bus disconnection, payment attribution, backup age, dependency degradation.
- `k8s/verify/` runs nine suites inside the cluster every night (journey, replicas, quota,
  status page, position dedup, capabilities, restore-drill age, and more). PASS beats the
  `verify` heartbeat monitor on status.meshsat.net; FAIL posts to the `alrt-meshsat-status`
  ntfy topic. A manual run is `kubectl -n meshsat-hub create job --from=cronjob/hub-verify hub-verify-manual-N`.
- `test:race` runs the race detector nightly in CI (schedule id 11); the regular jobs cannot,
  because production builds are `CGO_ENABLED=0`.

## Troubleshooting

| Symptom | Where to look |
|---------|---------------|
| `readyz` 503, mqtt unhealthy | `kubectl -n meshsat-hub logs nats-0`; the Hub keeps serving and alerts (`MeshSatHubBusDisconnected`) rather than dropping out of the Service |
| MQTT connect/disconnect flapping | duplicate `HUB_MQTT_CLIENT_ID`: it must carry the pod name |
| a replica reconnecting every 30 s after an SMS | a `+` in a topic segment; every builder must go through `hubmqtt.EncodeSegment` (MESHSAT-1022) |
| bridge shows offline while health flows | the reaper and stale-birth detection in `internal/bridge`; check `last_seen` and the tenant's `bridge_offline_timeout` |
| mTLS handshake timeout | `nats-certs` mounted and readable; the bridge presents a Hub-issued cert, system roots for the server side |
| certificate expired | reissue from the kit's page under Kits; NATS reloads its auth on SIGHUP from the reloader sidecar |
| 1 in 3 requests vanish with no HTTP response | a VPS edge whose IPsec tunnels are down silent-drops; probe each A record separately |
| a Secret changed and nothing happened | Reloader should have rolled `hub`; check its log for `Changes detected in 'hub-secrets'` |
| "slow query detected" in the log | the line carries `statement`; `pg_stat_statements` is loaded on the cluster for the full picture (MESHSAT-1155) |
