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
- **Placement rule: no Hub volume on `notrf01dmz06`** (81% disk, memory-saturated). Every
  PVC-bearing pod (hub, nats, redis) carries a `required` nodeAffinity `hostname NotIn
  [notrf01dmz06]` + worker. `preferred` guarantees nothing: LocalPV binds permanently on first
  schedule. CNPG runs on the control-plane tier.
- Replicas: hub x1 + `Recreate` until the single-writer work is proven on the cluster (plan
  phase 7), then x2 + PDB. NATS/Redis/stunnel x1.

## Config & secrets

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
