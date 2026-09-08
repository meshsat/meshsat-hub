# meshsat-hub/k8s — authoring conventions (MESHSAT-864 / MESHSAT-930)

Kustomize base for MeshSat Hub on `notrf01cl01k8s`. Synced by ONE Argo CD Application
(`meshsat-hub`, NO infra repo `k8s/argocd-apps/meshsat-hub/`, `prune: false`, `selfHeal: true`).
OpenTofu never touches anything here (platform/app split, see the NO repo's `k8s/CLAUDE.md`).
Modelled on `websites/omoikane.coach/daemon/k8s/`; where the two differ, this file wins here.

## Layout

Root `kustomization.yaml` lists `namespaces.yaml` + one directory per service; each directory
has its own `kustomization.yaml`. A service dir contains, as applicable: `deployment.yaml` /
`statefulset.yaml` / `cluster-*.yaml`, `service.yaml`, `externalsecret.yaml`, `configmap.yaml`,
`ingress.yaml`, `pvc.yaml`.

## Namespaces

- `meshsat-hub` — all workloads (hub, nats, redis, stunnel, the edge relay) and the
  `auth.meshsat.net` Ingress.
- `meshsat-hub-db` — the CNPG Cluster only. DB pods carry control-plane tolerations; app pods
  NEVER do.
- `monitoring` — ServiceMonitors and the metrics-token ExternalSecret (per-object namespace).

## Naming & labels

- Workload/Service name = short service name (`hub`, `nats`, `nats-ws`, `redis`, `stunnel`,
  `meshsat-edge-relay`, `authentik-upstream`). In-cluster DNS is `<name>.meshsat-hub.svc`.
- Every object: `app.kubernetes.io/name: <name>`. Root kustomize adds
  `app.kubernetes.io/part-of: meshsat-hub` + `environment: production` (not on selectors).
- Selectors use `app.kubernetes.io/name` only.

## Pods

- `serviceAccountName: meshsat-hub` (in `platform/`, carries `imagePullSecrets: registry-meshsat`
  because `ghcr.io/meshsat/*` is private). `automountServiceAccountToken: false` except the Hub
  (Lease election + bridge-CA Secret writer; RBAC in `platform/rbac.yaml`).
- Non-root: the Hub and stunnel run as uid/gid 65532 via `securityContext`.
- Resources: `requests.memory` = observed RSS rounded up, `limits.memory` = a ceiling,
  `requests.cpu` 20m–200m, **no CPU limits**.
- Probes: Hub `startupz`/`readyz`/`healthz`; NATS `/healthz` on the monitor port; Redis
  `redis-cli ping` with `REDISCLI_AUTH` (password never in argv); TCP for stunnel/relay.
- **Placement rule: stateful on the control-plane tier, stateless on the workers**
  (rewritten 2026-09-08, MESHSAT-711). Anything holding a local volume (CNPG, NATS, KeyDB) runs
  on dmz03/04/05 with `nodeSelector: node-role.kubernetes.io/control-plane` plus the matching
  toleration and **required** hostname anti-affinity. Anything stateless (hub, stunnel, edge
  relay) runs on the workers. The reason is capacity, measured: the workers are 49, 90 and 92
  percent committed by other namespaces while the control-plane nodes sit at 1, 2 and 0.
  Anti-affinity must be `required` and not `preferred`, because a local volume binds permanently
  on first schedule, so `preferred` can put two members of a three-member quorum on one machine
  and lose the group when it dies. The old rule, "no Hub volume on dmz06", is retired: dmz06 and
  dmz01 are now within three percentage points of each other on disk, and the Hub has no volume
  at all since the audit archive moved to the object store.
- **Tolerations**: stateless pods never carry control-plane tolerations. Stateful ones do, and
  in exchange must carry a memory limit and `priorityClassName: meshsat-hub-critical` so they
  can never starve etcd or the API server.
- **Priority**: everything in the Hub's data path carries `meshsat-hub-critical`
  (`k8s/platform/priorityclass.yaml`), queue priority with no preemption. Without it a
  replacement pod competes on equal terms with tens of gigabytes of other namespaces' workloads
  at the moment a node dies. A Pending Hub pod during a failure drill is the signal to raise
  preemption, deliberately, with the omoikane owner.
- Replicas: hub x2 across all three workers (`RollingUpdate` maxSurge 1 / maxUnavailable 0, PDB
  `minAvailable 1`, required podAntiAffinity, 60s not-ready tolerations); NATS x3, one per
  control-plane node, PDB `minAvailable 2` because a Raft group must never be drained below
  quorum; KeyDB x2 on the control-plane tier, PDB `minAvailable 1`; stunnel x2 on the workers,
  PDB `minAvailable 1`.
- **Failure domains**: losing any one machine leaves the Hub serving, the database with a
  primary and a synchronous replica, and the broker and cache with quorum. In-cluster failover
  has a floor of roughly 45 to 60 seconds, because a dead node is not marked NotReady before
  then; the external edge fails a dead worker over in about 4 to 10 seconds. Losing the whole
  site is not covered: all six machines are one site with no zone labels.
- Non-secret env → `<name>-config` ConfigMap (`envFrom`).
- Secrets → ExternalSecret `<name>-secrets`, `secretStoreRef {name: openbao, kind:
  ClusterSecretStore}`, `refreshInterval: 1h`, `creationPolicy: Owner`, **`deletionPolicy:
  Retain`**, remoteRef `ci-no/apps/meshsat-hub/<service>` (`hub`, `nats`, `redis`, `cnpg`,
  `registry`). Connection strings are TEMPLATED in `hub/externalsecret.yaml` from the sibling
  secrets so they cannot drift.
- Exception: `meshsat-bridge-ca` has `refreshInterval: "0"` (seeded once from
  `nats/bridge_ca_crt`) because the Hub patches it at runtime.
- TLS: `meshsat-net-tls` from `k8s/shared/wildcard-meshsat-net-tls` (NL cert-manager PushSecret;
  self-renewing). Consumers: both Ingresses, NATS (reloaded), stunnel (rollout restart).

## Database (CNPG)

- `meshsat-hub-main-rw.meshsat-hub-db.svc:5432`, database `meshsat_hub`, role `meshsat`,
  `sslmode=require`. 3 instances, `synchronous_commit on`, 1 sync replica, 10Gi
  `local-hostpath-retain` per instance, barman to `s3://cnpg-meshsat-hub` on
  `nl-s3.nuclearlighters.net`, retention 14d, daily base backup 02:45 (6-field cron).
- Restore drill before cutover: throwaway `Cluster` with `bootstrap.recovery` from the bucket,
  row counts against live, delete (NOTES.md).

## Public path

- HTTPS: VPS HAProxy → node mesh IP `:8443` (omoikane's edge-relay) → ingress-nginx → Ingress
  `hub.meshsat.net` / `auth.meshsat.net` (`ingressClassName: nginx`, `tls:` block with
  `meshsat-net-tls`; the edge re-encrypts with `verify required ... sni`).
- MQTT (wss, bridge client certs): VPS HAProxy TCP passthrough → node mesh IP `:9443`
  (`meshsat-edge-relay`, hostNetwork) → `nats-ws:9443` (NATS terminates TLS + verifies the
  bridge certificate against `meshsat-bridge-ca`).
- Reticulum: same → `:4243` → `stunnel:4243` (TLS + client cert) → `hub:4242` (HDLC).
- `/metrics` on the public Ingress is routed to the endpoint-less `hub-metrics-deny` Service
  (503); Prometheus scrapes in-cluster with the bearer token.
- `auth.meshsat.net` is the shared authentik (namespace `omoikane`) through the ExternalName
  `authentik-upstream` with `proxy-cookie-domain: "omoikane.coach meshsat.net"`.

## Images

- Referenced by bare name; tags/digests pinned ONLY in the root `kustomization.yaml`
  `images:` block. The Hub digest is rewritten by the `bump_k8s_pin` CI job on every main
  pipeline (`k8s/scripts/bump-pin.py`). `meshsat-hub-stunnel` is built by `package:stunnel`
  from `k8s/stunnel/Dockerfile`.
- `kustomize build k8s/ | kubeconform` runs in CI (`kubeconform` job) on every change here.
