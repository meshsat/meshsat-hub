# hawkBit — the platform's own OTA server

Deployed for MESHSAT-1121. Until then `HUB_HAWKBIT_ENABLED` was `false` and no
hawkBit existed anywhere in the estate, so "OTA" was a nav entry in front of
nothing.

**This is the PLATFORM's server, serving the default tenant only.** Every other
tenant supplies its own on the Integrations page. See `internal/hawkbit/pool.go`.

## hawkBit is multi-tenant itself, and that is the whole design

Its Management API scopes every request to the tenant of the authenticated
principal. So "which tenant" is carried entirely by the credentials: the Hub
needed no tenant field on Target, Rollout or controllerId, and no tenant
parameter on the Go client. A tenant that points the Hub at THIS server with
its own hawkBit account is isolated by hawkBit, not by us.

That is worth stating because OTA is the highest-impact primitive on the router:
it pushes firmware to hardware in the field. Before MESHSAT-1121 there was one
server, one account and no ownership model anywhere, which is why MESHSAT-1116
had to gate the endpoints to platform admins.

## Placement and weight

Control-plane tier (dmz03/04/05), measured at 6-7% memory on 2026-09-14 while the
worker tier sat at 90% on two of three nodes. hawkBit is a Spring Boot JVM and is
genuinely heavy — ~1.5Gi with its heap capped — which is why the tier with the
headroom is not optional here the way it was for wg-easy.

It uses the bundled H2 database on a node-local volume rather than a Postgres of
its own. That is a deliberate limit, not an oversight: this instance serves the
default tenant's own kit, a handful of devices. A tenant running OTA at fleet
scale brings its own hawkBit with its own database, which is exactly what the
per-tenant provider is for. If the platform's own usage ever outgrows H2, give it
a CNPG cluster beside meshsat-hub-main rather than scaling H2.

## Reachability is a probe, not a gate

`IsReachable` used to decide at startup whether the OTA routes existed at all, so
a hawkBit that was merely DOWN at boot disabled the feature permanently with no
retry. It is a health probe now and the routes exist either way.

## The database is Postgres, on the shared CNPG cluster (MESHSAT-1131)

hawkBit shipped here on the bundled H2: one file on a node-local volume, no
backup, at whatever version the image happened to carry. That is how OTA broke
on day one — H2 2.x removed `IDENTITY()`, which hawkBit's EclipseLink layer
emits, so **every authenticated REST call answered 500** while the pod sat
`1/1 Running` and `/actuator` answered. Nothing but the Hub's dependency probe
could tell; `MeshSatHubDependencyDegraded` was firing for `hawkbit` on both
replicas within an hour of the bus rule going live (MESHSAT-1129).

`MODE=LEGACY` held it for an evening. The fix is the database it should have
had: `hawkbit` on `meshsat-hub-main`, owned by its own `hawkbit` role
(`k8s/db/database-hawkbit.yaml`, `cluster-meshsat-hub-main.yaml`), reached via
`meshsat-hub-main-rw.meshsat-hub-db.svc:5432` with `sslmode=require`. barman
backs it up to S3 nightly with everything else, it survives a node, and it does
not change under a manifest that did not.

How it is told: the image ships a `postgresql` Spring profile that sets
`spring.jpa.database` and the driver class, so `SPRING_PROFILES_ACTIVE=postgresql`
plus `SPRING_DATASOURCE_URL/USERNAME/PASSWORD`. The password is ONE OpenBao
property (`ci-no/apps/meshsat-hub/hawkbit` → `HAWKBIT_DB_PASSWORD`) delivered
to both namespaces — CNPG's `db-role-hawkbit` Secret and the pod's
`hawkbit-secrets` — so role and client cannot disagree.

The volume stayed and changed jobs: it is the **artifact store** now
(`ORG_ECLIPSE_HAWKBIT_REPOSITORY_FILE_PATH=/var/lib/hawkbit/artifacts`), because
hawkBit's default artifact path is relative to the working directory, i.e. the
container's ephemeral layer, and an uploaded firmware image would vanish on the
next pod replacement. The old `data.mv.db` beside it is dead: no rollout ever
succeeded on H2, so there was nothing to migrate.

**The image is pinned by digest**, not `latest` — an untagged image is how the
bundled database changed underneath a manifest that did not.

Prove it with an authenticated call from a Hub pod, never with pod status:

```sh
kubectl -n meshsat-hub exec deploy/hub -c hub -- sh -c '
  A=$(printf "%s:%s" "$HUB_HAWKBIT_USERNAME" "$HUB_HAWKBIT_PASSWORD" | base64 -w0)
  wget -S -O /dev/null --header="Authorization: Basic $A" http://hawkbit:8080/rest/v1/targets 2>&1 | grep -o "HTTP/1.1 [0-9]*"'
```
