# Constitution — MeshSat Hub (project 35)

17 non-negotiable principles for the multi-tenant SaaS platform. **Security is #1**, above features, performance, and convenience. These bind every contributor — human or agent. The merge-coordinator + validate-project-spec.py + CI security stage enforce them.

> Companion documents: vision/mission/scope live in [`PROJECT.md`](PROJECT.md). Decisions live in `adr/`. Roadmap in `docs/ROADMAP.md`. Runbook in `docs/RUNBOOK.md`. Wire protocol in `docs/bridge-hub-protocol.md`. Encryption model in `docs/ENCRYPTION.md`. Deployment per tier in `docs/deployment.md`.

## Article I — Identity boundary (NON-NEGOTIABLE)

The system shall be the multi-tenant SaaS management platform and NOTHING ELSE. Bridge code (serial AT commands, direct Meshtastic proto, USB hotplug, ZigBee, GPIO) belongs in `meshsat/` (project 27). Companion-app code (Compose UI, Android paths) belongs in `meshsat-android/` (project 31). Before every commit, verify `pwd` shows `meshsat-hub/` and `git remote -v` shows project 35.

## Article II — Security is #1 (above features, performance, convenience)

The system shall treat every code change, dependency addition, and deployment through a security lens. **A secure system that's missing a feature is better than a feature-rich system with a vulnerability.** This article wins every priority tie.

## Article III — Zero trust on all inputs

The system shall validate + sanitize every external input. Every HTTP handler uses the strict-decode helper. The canonical implementation lives in `internal/httpjson/readjson.go` as `httpjson.ReadJSON()`; an in-package wrapper `internal/api/readJSON()` delegates to it for the 43 existing callers inside `internal/api/`. **Sibling packages (`internal/wireguard`, `internal/globalstar`, `internal/webhook`, ...) MUST import `internal/httpjson` directly** — they cannot import `internal/api` because `internal/api` already imports `internal/wireguard` for auto-provisioning, creating an import-cycle risk. All three call sites enforce: size-limit (default 1 MB), `DisallowUnknownFields()`, multi-value rejection (`dec.More()`), typed-error unwrapping (SyntaxError / UnmarshalTypeError / MaxBytesError / "unknown field" / EOF). Webhook payloads are HMAC-verified BEFORE processing. SQL queries use `?` parameterised binds — NEVER string concatenation. **`json.NewDecoder(r.Body).Decode()` without size-limit + `DisallowUnknownFields()` is FORBIDDEN**.

## Article IV — Triple-layer tenant isolation

The system shall enforce tenant isolation at three layers simultaneously:
- **Storage:** SQLite-file-per-tenant (Tier 1) or schema-partitioning (Tier 2+)
- **Transport:** MQTT topic ACL prefix `meshsat/{tenant_id}/{device_id}/...`
- **Identity:** JWT `tenant_id` claim propagation via `TenantContextKey`

Every method on the Store interface (`internal/store/store.go` lines 24-194) takes `tenantID string` as its second parameter. A single bad SQL query that drops `WHERE tenant_id` leaks cross-tenant. NO EXCEPTIONS.

## Article V — `CGO_ENABLED=0` everywhere

The system shall compile pure-Go. Use `modernc.org/sqlite`, not the CGO sqlite driver. This is what blocks FIPS-140-3 BoringSSL (MESHSAT-573) without a separate build target.

## Article VI — SMAZ2 dictionary byte-for-byte mirror

The system shall use a SMAZ2 vocabulary dictionary IDENTICAL to `meshsat/internal/compress/dict_meshtastic.go`. Independent modification = decompression failures for every field-originated message. Verify byte-for-byte before any compression-related merge.

## Article VII — Emergency Control Plane isolation

The system shall NEVER be in the critical path for ESP32 field-device emergency operations. Hub may receive and process ECP events but must never block them. SOS, dead-man-switch, and geofence-trigger paths must fail-OPEN if Hub is unreachable.

## Article VIII — Audit log is tamper-evident (SHA-256 hash chain)

The system shall write all security-relevant events to `internal/audit/audit.go` as a SHA-256 hash chain. Append-only. Any in-place modification breaks chain verification. The retention goroutine archives entries to JSONL BEFORE purging — JSONL archive path MUST exist before retention runs or entries are lost silently.

## Article IX — Webhook authenticity verified BEFORE processing

The system shall verify the authenticity of every inbound webhook payload BEFORE any business-logic processing. The preferred mechanism is HMAC-SHA256 signature verification, used for `/api/webhook/rockblock` and `/api/webhook/globalstar`. For providers that do not offer HMAC signatures — currently **Cloudloop**, whose only documented webhook-authenticity mechanism is IP-based (per [`knowledge.cloudloop.com/docs/destinations/http`](https://knowledge.cloudloop.com/docs/destinations/http), verified 2026-05-17) — verification is performed via a strict IP allowlist scoped to the provider's published outbound IPs (`config.DefaultCloudloopWebhookIPs` for Cloudloop: `35.178.100.117`, `52.56.155.169`; operator-overridable via `HUB_CLOUDLOOP_WEBHOOK_ALLOWED_IPS`). Regardless of which auth layer applies, every inbound webhook handler MUST also enforce Article III (strict JSON decode via `httpjson.ReadJSON`) and a body size limit. Skipping any verification layer = arbitrary inbound MO injection. If Cloudloop ever publishes an HMAC signature mechanism, this carve-out collapses back to "HMAC for all webhooks."

## Article X — Bridge lifecycle: reaper AND stale-birth detection BOTH required

The system shall mark bridges offline via BOTH the timeout-based Reaper AND 5-min-threshold stale-birth detection. Without stale-birth detection, every Hub restart defeats the reaper and dead bridges reappear from broker-retained births.

## Article XI — Galera operational rules

The system shall NEVER modify `WSREP_CLUSTER_ADDRESS` in `.env` (use the flag-file bootstrap mechanism). NEVER run `docker compose up -d` without `--no-deps` (recreates MariaDB). NEVER `docker compose pull` (evaluates all services). NEVER deploy if `scripts/check-galera-health.sh` fails. The 15 numbered safety layers in `scripts/galera-entrypoint.sh` exist because of incidents 11/13/14 — do NOT bypass. See ADR-0003.

## Article XII — Single-source-of-truth migrations

The system shall update BOTH the `internal/store/sqlite/*.go` files (currently split across `sqlite.go` + `bond_groups.go` + `bridges.go` for clarity; new feature-tables get their own file under `sqlite/`) AND `internal/store/mariadb/mariadb.go` (single 2198-line file) for ANY schema change. The hand-rolled `CREATE IF NOT EXISTS` + `ALTER IGNORE DUPLICATE` pattern means a missed update silently diverges schemas between tiers. Today's parity is verified by feature-name presence in both stores (e.g. `bond_groups` appears in both `sqlite/bond_groups.go` and 7 times in `mariadb/mariadb.go`).

## Article XIII — Vue 3 Composition API + Tailwind only

The system shall use Vue 3 Composition API exclusively (no Options API), Tailwind CSS exclusively (no other CSS framework), Pinia for state. Port allocation: Hub on 6070, MQTT on 6071/6072 (6xxx scheme).

## Article XIV — Security operational rules

The system shall: (a) require ADR justification for every new third-party dep (license check, `govulncheck` CVE count, maintenance signal, transitive dep count, prefer stdlib, pin versions); (b) NEVER commit secrets — env vars or operator-provisioned config files only, backups MUST redact, logs NEVER contain tokens/keys/passwords; (c) run `make lint && make test && gosec ./... && govulncheck ./...` on every push with HIGH-severity findings BLOCKING merge; (d) OWASP ZAP every public URL at each v0.X release; (e) a task is NOT done until pipeline passes AND deployment is verified.

## Article XV — Parallel-dev gates

The system shall enforce that no two parallelizable tasks share `files_owned` entries within the same dependency wave — validated by `bootstrap-pack/scripts/validate-project-spec.py` at the Phase F gate BEFORE any worker launches. The repo's default workflow ("push directly to main, no branches, no MRs") is overridden for parallel-dev waves: ONE short-lived `merge/<feature_id>` branch, ONE MR per feature, auto-delete on merge. See ADR-0004.

## Article XVI — Tri-mode deployment invariants

The system shall support exactly three deployment shapes, selected at startup via `HUB_MODE`: `standalone` (SQLite + Mosquitto), `cluster` (MariaDB Galera + NATS + Redis), `kubernetes` (StatefulSets + Lease-API leader election). The binary is single — mode-specific behavior hides behind `Store`, `Bus`, `Leader`, and dedup/ratelimit interfaces. `internal/config/` MUST refuse to start on unknown `HUB_MODE` values. The `/api/cluster/node` + `/api/cluster/status` endpoints MUST report which mode is active. Schema parity between `internal/store/sqlite/*.go` and `internal/store/mariadb/mariadb.go` (Article XII) is what keeps mode substitutability honest — drift breaks the guarantee. See ADR-0005 and `docs/ROADMAP.md` L10–L31.

## Article XVII — Bridge⇄Hub wire-protocol contract

The system shall implement the `meshsat-uplink/v1` Sparkplug-B-inspired protocol on the bridge ⇄ Hub uplink: BIRTH (retained, QoS 1) → DATA → DEATH (LWT-backed, QoS 1) for both bridges and the devices they own. Every message MUST include `"protocol": "meshsat-uplink/v1"` + an RFC-3339 `timestamp` field. Topic namespace is `meshsat/bridge/{bridge_id}/{birth|death|health|cmd|cmd/response}` for bridge-level and `meshsat/bridge/{bridge_id}/device/{device_id}/{birth|death}` for device-level. Bridges MUST set their MQTT LWT BEFORE publishing birth. Hub's bridge reaper MUST handle BOTH the timeout AND the 5-min stale-birth path (Article X). Schemas live in `internal/protocol/protocol.go` (Hub) and `meshsat/internal/hubreporter/protocol.go` (Bridge) — they are NOT a shared module; kept in lockstep via the `protocol` version field. Any breaking change to topic shape requires a `meshsat-uplink/v2` namespace, NOT an in-place edit. See ADR-0006 and `docs/bridge-hub-protocol.md`.
