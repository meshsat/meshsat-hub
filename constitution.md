# Constitution — MeshSat Hub (project 35)

17 non-negotiable principles for the multi-tenant SaaS platform. **Security is #1**, above features, performance, and convenience. The merge-coordinator + validate-project-spec.py + CI security stage enforce them.

> Companion documents: vision/mission/scope live in [`PROJECT.md`](PROJECT.md). Decisions live in `adr/`. Roadmap in `docs/ROADMAP.md`. CGC-grounded 2026-05-18.

## Article I — Identity boundary

The system shall be the multi-tenant SaaS management platform and NOTHING ELSE. Bridge code (serial AT commands, direct Meshtastic proto, USB hotplug, ZigBee, GPIO) belongs in `meshsat/` (project 27). Companion-app code (Compose UI, Android paths) belongs in `meshsat-android/` (project 31). Before every commit, verify `pwd` shows `meshsat-hub/` and `git remote -v` shows project 35.

## Article II — Security is #1

The system shall treat every code change, dependency addition, and deployment through a security lens. A secure system missing a feature is better than a feature-rich system with a vulnerability. This article wins every priority tie.

## Article III — Zero trust on all inputs

The system shall validate + sanitize every external input. Every HTTP handler shall use the strict-decode helper at `internal/httpjson/readjson.go` (CGC-verified) — `httpjson.ReadJSON()`. Sibling packages (`internal/wireguard`, `internal/globalstar`, `internal/webhook`, `internal/cloudloop`) MUST import `internal/httpjson` directly. Webhook payloads MUST be HMAC-verified BEFORE processing (where the provider supports HMAC — see Article IX for Cloudloop carve-out). SQL queries use `?` parameterised binds — NEVER string concatenation.

## Article IV — Triple-layer tenant isolation

The system shall enforce tenant isolation at three layers simultaneously:
- **Storage:** SQLite-file-per-tenant (Tier 1) or schema-partitioning (Tier 2+)
- **Transport:** MQTT topic ACL prefix `meshsat/{tenant_id}/{device_id}/...`
- **Identity:** JWT `tenant_id` claim propagation via `TenantContextKey`

Every method on the Store interface (`internal/store/store.go`) takes `tenantID string` as its second parameter. NO EXCEPTIONS.

## Article V — `CGO_ENABLED=0` everywhere

The system shall compile pure-Go. Use `modernc.org/sqlite`, not the CGO sqlite driver. This blocks FIPS-140-3 BoringSSL until a separate build target.

## Article VI — SMAZ2 dictionary byte-for-byte mirror

The system shall use a SMAZ2 vocabulary dictionary IDENTICAL to `meshsat/internal/compress/dict_meshtastic.go`. Independent modification = decompression failures for every field-originated message.

## Article VII — Emergency Control Plane isolation

The system shall NEVER be in the critical path for ESP32 field-device emergency operations. Hub may receive and process ECP events but must never block them. SOS, dead-man-switch, and geofence-trigger paths must fail-OPEN if Hub is unreachable.

## Article VIII — Audit log is tamper-evident (SHA-256 hash chain)

The system shall write all security-relevant events to `internal/audit/` as a SHA-256 hash chain. Append-only. The retention goroutine archives entries to JSONL BEFORE purging.

## Article IX — Webhook authenticity verified BEFORE processing

The system shall verify the authenticity of every inbound webhook payload BEFORE any business-logic processing. The preferred mechanism is HMAC-SHA256 signature verification (used for `/api/webhook/rockblock` and `/api/webhook/globalstar`). For providers that do not offer HMAC signatures — currently **Cloudloop**, whose only documented webhook-authenticity mechanism is IP-based (per [`knowledge.cloudloop.com/docs/destinations/http`](https://knowledge.cloudloop.com/docs/destinations/http)) — verification is performed via a strict IP allowlist scoped to the provider's published outbound IPs (`config.DefaultCloudloopWebhookIPs` for Cloudloop: `35.178.100.117`, `52.56.155.169`; operator-overridable via `HUB_CLOUDLOOP_WEBHOOK_ALLOWED_IPS`). Real implementation: `internal/config/config.go` `ResolveCloudloopAllowedIPs` (CGC-verified). Every inbound webhook handler MUST also enforce Article III + body size limit. If Cloudloop ever publishes HMAC, this carve-out collapses.

## Article X — Bridge lifecycle: reaper AND stale-birth detection BOTH required

The system shall mark bridges offline via BOTH the timeout-based Reaper AND 5-min-threshold stale-birth detection. Without stale-birth detection, every Hub restart defeats the reaper and dead bridges reappear from broker-retained births.

## Article XI — Galera operational rules

The system shall NEVER modify `WSREP_CLUSTER_ADDRESS` in `.env` (use the flag-file bootstrap mechanism). NEVER run `docker compose up -d` without `--no-deps`. NEVER `docker compose pull`. NEVER deploy if `scripts/check-galera-health.sh` fails. The numbered safety layers in `scripts/galera-entrypoint.sh` exist because of past incidents.

## Article XII — Single-source-of-truth migrations

The system shall update BOTH `internal/store/sqlite/*.go` (CGC-verified: sqlite.go, bridges.go, bond_groups.go; new feature-tables get their own file under sqlite/) AND `internal/store/mariadb/mariadb.go` for ANY schema change. The hand-rolled `CREATE IF NOT EXISTS` + `ALTER IGNORE DUPLICATE` pattern means a missed update silently diverges schemas between tiers.

## Article XIII — Vue 3 Composition API + Tailwind only

The system shall use Vue 3 Composition API exclusively, Tailwind CSS exclusively, Pinia for state. Port allocation: Hub on 6070, MQTT on 6071/6072.

## Article XIV — Security operational rules

The system shall: (a) require ADR justification for every new third-party dep; (b) NEVER commit secrets; (c) run `make lint && make test && gosec ./... && govulncheck ./...` on every push with HIGH-severity blocking merge; (d) OWASP ZAP every public URL at each v0.X release; (e) a task is NOT done until pipeline passes AND deployment is verified.

## Article XV — Parallel-dev gates

The system shall enforce that no two parallelizable tasks share `files_owned` entries within the same dependency wave. The repo's default workflow is overridden for parallel-dev waves: ONE short-lived `merge/<feature_id>` branch, ONE MR per feature, auto-delete on merge.

## Article XVI — Tri-mode deployment invariants

The system shall support exactly three deployment shapes, selected at startup via `HUB_MODE`: `standalone` (SQLite + Mosquitto), `cluster` (MariaDB Galera + NATS + Redis), `kubernetes` (StatefulSets + Lease-API leader election). The binary is single — mode-specific behavior hides behind `Store`, `Bus`, `Leader`, and dedup/ratelimit interfaces. `internal/config/` MUST refuse to start on unknown `HUB_MODE` values. The `/api/cluster/node` + `/api/cluster/status` endpoints MUST report which mode is active.

## Article XVII — Bridge⇄Hub wire-protocol contract

The system shall implement the `meshsat-uplink/v1` Sparkplug-B-inspired protocol on the bridge ⇄ Hub uplink: BIRTH (retained, QoS 1) → DATA → DEATH (LWT-backed, QoS 1) for both bridges and the devices they own. Every message MUST include `"protocol": "meshsat-uplink/v1"` + an RFC-3339 `timestamp` field. Topic namespace is `meshsat/bridge/{bridge_id}/{birth|death|health|cmd|cmd/response}` for bridge-level and `meshsat/bridge/{bridge_id}/device/{device_id}/{birth|death}` for device-level. Bridges MUST set their MQTT LWT BEFORE publishing birth. Schemas live in `internal/protocol/protocol.go` (Hub) and `meshsat/internal/hubreporter/protocol.go` (Bridge) — they are NOT a shared module; kept in lockstep via the `protocol` version field. Any breaking change to topic shape requires a `meshsat-uplink/v2` namespace.
