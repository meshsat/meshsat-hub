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

## `MODE=LEGACY` on the H2 URL is not cruft (MESHSAT-1131)

hawkBit's EclipseLink layer emits the H2 function `IDENTITY()`, which H2 2.x
removed. Without `;MODE=LEGACY` on `SPRING_DATASOURCE_URL`, **every
authenticated REST call answers 500** with

```
JdbcSQLSyntaxErrorException: Function "IDENTITY" not found
```

while the pod is `1/1 Running` and `/actuator` answers. OTA was non-functional
from the day it was deployed and nothing but the Hub's dependency probe could
tell — `1/1 Running` says nothing about whether the database works; only a
REST call does. It was found because `MeshSatHubDependencyDegraded` was firing
for `dependency=hawkbit` on both replicas within an hour of the bus rule going
live (MESHSAT-1129).

Two things follow:

- **The image is pinned by digest**, not `latest`. The bundled H2 is whatever
  the image ships; an untagged image lets it move on any pod replacement with
  no commit to point at.
- **Postgres is the proper home** for this (CNPG is already on the cluster and
  hawkBit supports it natively) and is filed on MESHSAT-1131. Until that lands,
  removing `MODE=LEGACY` as tidy-up re-breaks OTA silently.

Prove it with an authenticated call from a Hub pod, never with pod status:

```sh
kubectl -n meshsat-hub exec deploy/hub -c hub -- sh -c '
  A=$(printf "%s:%s" "$HUB_HAWKBIT_USERNAME" "$HUB_HAWKBIT_PASSWORD" | base64 -w0)
  wget -S -O /dev/null --header="Authorization: Basic $A" http://hawkbit:8080/rest/v1/targets 2>&1 | grep -o "HTTP/1.1 [0-9]*"'
```
