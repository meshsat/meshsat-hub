# MeshSat Hub — security posture

_Assessed 2026-09-16 / 17. Instrument for the hardening programme MESHSAT-1189 → 1194._

## What this document is, and what it is not

It is a **self-assessment against two published standards**, OWASP ASVS 5.0 Level 2 for the
application and the CIS Kubernetes Benchmark chapter 5 for the workloads, scored by a rubric that is
written down below so the number can be argued with. It is **not** an external audit and no third
party has reviewed it.

It supersedes `docs/SECURITY_AUDIT.md`, which was dated 2026-03-18, described a `v0.2` product on
Docker Compose with MariaDB, and contained at least four claims contradicted by the CI configuration
it cited. That document should be read as history.

### Evidence standard

Every verdict below rests on one of three things, and the difference matters:

| mark | meaning |
|---|---|
| **measured** | a command was run against production and its output is quoted in the issue |
| **read** | the code or manifest was read at a named path |
| **unassessed** | nobody has looked; recorded as unknown, not as passing |

Where a control was believed to hold and did not, that is called out. Three of them were only found
because something was measured rather than reasoned about — see "What measurement caught" at the end.

## Scoring rubric

Per chapter: **1.0** for pass, **0.5** for partial, **0.0** for fail, chapters that do not apply are
excluded. Chapter-level granularity is coarse on purpose — it is honest about the resolution of the
assessment rather than implying a per-requirement audit that has not happened.

---

## OWASP ASVS 5.0 Level 2

| # | Chapter | Before | After | Basis for the verdict |
|---|---|:---:|:---:|---|
| V1 | Encoding & Injection | 0.5 | 0.5 | **read.** Every query parameterised; four `fmt.Sprintf` statements interpolate only closed-switch column names. No shell anywhere; `os/exec` is argv-form and platform-admin gated. Still open: CSV export does not neutralise formula injection (`internal/api/csv.go`), and ~99 handlers return raw driver error text to authenticated callers. |
| V2 | Validation & Business Logic | 0.5 | 0.5 | **read.** `readJSON` enforces a 1 MB cap, `DisallowUnknownFields` and single-value decoding — and is bypassed by six handlers, two without unknown-field rejection. `?limit=` is unbounded on eight list endpoints. **Fixed this round (MESHSAT-1202):** the Reticulum HDLC reassembly buffer grew without bound — a peer that sent one delimiter and never a second one chose the Hub's memory usage, reachable from the internet with any bridge-CA client certificate. Now capped at 16 KiB against a ~1002-byte theoretical maximum frame, with a regression test and a fuzz target both proven to fail without the fix. **Still 0.5**, because the `readJSON` bypasses and the unbounded `?limit=` are untouched, and `iface_tcp.go` still has no connection cap and still broadcasts every outbound frame to every client. |
| V3 | Web Frontend Security | 1.0 | 1.0 | **measured.** CSP with `script-src 'self'`, `object-src 'none'`, `frame-ancestors 'none'`; HSTS preload; XFO, nosniff, Referrer-Policy, Permissions-Policy, COOP, CORP all present on the live response. No CORS configured, which for a bearer-token API is correct. No CSP reporting. The Hub scored 1.0 here throughout — but **`auth.meshsat.net`, the identity provider, sent no CSP at all** until MESHSAT-1198, which is a reminder that scoring one host does not score the login path it depends on. Now fixed, with `'unsafe-inline'` as a named residual. |
| V4 | API & Web Service | 0.0 | 0.5 | **measured.** Was: 250 routes, 137 ungated, 45 of them state-changing. Now every mutating route carries a role floor, enforced by a build-time ratchet, and a viewer key returns 403 on each in production. Still 0.5: there is no rate limit on any *authenticated* route, and none on the two unauthenticated capability URLs. |
| V5 | File Handling | 1.0 | 1.0 | **read.** `assetKey` validates extension, segment count and each segment; backup import guards zip-slip via a cleaned-path prefix check; export uses `os.OpenRoot`. Zip-bomb entry/size limits absent, platform-admin only. |
| V6 | Authentication | 0.0 | **0.5** | **measured.** `HUB_AUTH_TOKEN` is a static, never-expiring bearer granting `PlatformAdmin: true` in every auth mode, present in the production secret. Not deleted: every consumer needs the platform axis and an API key cannot carry it, so removing this would mean weakening that guarantee (MESHSAT-1195). Now **monitored** instead — counted, logged with the resolved client IP, written to the audit chain, refused on the onion channel indistinguishably from a wrong token, and alerted on. 0.5 for compensating controls, not 1.0: still static, still non-expiring, still rotated only by redeploy. |
| V7 | Session Management | 0.5 | 0.5 | **read + measured.** 15-minute HS256 access tokens, rotated single-use refresh tokens stored SHA-256 hashed, `SameSite=Strict`. Fixed: logout was not auth-exempt, so an expired session could not revoke its own 7-day refresh token — and exempting it alone would have cleared the cookie while revoking nothing. Still open: no access-token revocation, and the refresh cookie's `Secure` flag derives from a client-influenceable header. |
| V8 | Authorization | 0.5 | **1.0** | **measured.** `internal/store/scoping.go` is a two-sided ratchet failing the build for any store method or SQL statement that loses its tenant, with 54/74 written-down exemptions; no IDOR found. Now joined by `internal/auth/routefloor.go`, the same shape for route authorisation. Residual, tracked: `internal/crypto.KeyStore` is keyed by IMEI with no tenant dimension (durable rows are scoped; the process cache is not), and `api_keys.device_imei` is stored as if it were a scope and never read. |
| V9 | Self-contained Tokens | 1.0 | 1.0 | **read.** Algorithm allowlist, `exp` required, audience and issuer checked, `kid` required with a single JWKS refresh, OKP and symmetric keys rejected, EC points verified on-curve, 1 MB response caps. Claims are discarded and role/tenant re-read from the Hub's own tables. |
| V10 | OAuth & OIDC | 1.0 | 1.0 | **read.** PKCE S256, HMAC-signed state cookie with its own expiry, nonce checked against the ID token, open-redirect guard, `email_verified` required before provisioning, SPKI pinning with a rotation backup pin. |
| V11 | Cryptography | 1.0 | 1.0 | **read.** AES-256-GCM with a 12-byte random nonce, bcrypt cost 10, five long-lived keys sealed under `HUB_CONFIG_WRAP_KEY` with a preflight that refuses to boot rather than regenerate. No forward secrecy on the satellite path, documented with a cost argument in `docs/ENCRYPTION.md`. |
| V12 | Secure Communication | 0.5 | 0.5 | **measured.** TLS 1.2/1.3 at the edge; NATS websocket and stunnel both verify client certificates against the bridge CA. Improved: every NATS listener — including the cluster route port, which has no authorization block and no TLS — and the Postgres cluster are no longer reachable from arbitrary pods. Not 1.0: in-cluster MQTT is still plaintext within the allowed set, and NATS route authentication is still absent — contained now rather than fixed. Separately, the client-certificate control on the two TLS-passthrough endpoints is now **asserted nightly from two external vantages** (MESHSAT-1200) — it had not been checked since 2026-08-04. |
| V13 | Configuration | 1.0 | 1.0 | **read.** Every secret an ExternalSecret from OpenBao; no secret values committed; `"changeme"` appears only as a value to reject. An IMEI and a Cloudloop thingId sit in a ConfigMap — sensitive, not secret. |
| V14 | Data Protection | 0.5 | **1.0** | **measured.** Tenant export, redaction on export, audit retention bounded 30–3650 days and tenant-selectable. The cross-tenant TAK leak (MESHSAT-1032) is closed: the platform-wide CoT gateway is gone, replaced by a per-tenant forwarder that resolves the tenant off the topic and reaches only that tenant's upstreams, and the unscoped `/api/tak/federation/peers` route and TAK Operations page no longer exist. Now held by three tests using `DefaultTenantID` as the victim, proven by reintroducing the bug in production code. Every other `DualFilters` consumer was audited for the same shape: sos, position, message and mesh all resolve through `tenancy.Resolver`, which is stronger than topic parsing because the store is authoritative. Residual, other repo: the privacy page has not been checked against what the Hub actually does with location data. |
| V15 | Secure Coding & Architecture | 1.0 | 1.0 | **read.** Invariants held by tests that are declared not to be weakened (SOS survives quota; quota is on no ingest path; refunds are on no ingest path). Ratchets rather than review as the enforcement mechanism. |
| V16 | Security Logging & Error Handling | 0.0 | **1.0** | **measured.** Was: a 401 produced no metric, no log line and no audit row, because auth is registered outside metrics and logging and short-circuits; rejection reasons were logged at Debug while production runs at info. Now every refusal increments a labelled counter and emits a `Warn` line with the correctly-resolved client IP — proven 0 → 6 on real production 401s, and `ip=45.138.52.48` rather than the ingress pod. |
| V17 | WebRTC | — | — | Not applicable. |

**ASVS L2: 11.0 / 16 = 69% → 14.0 / 16 = 88%**

---

## CIS Kubernetes Benchmark, chapter 5 (workload scope)

Chapters 1–4 cover the control plane and node configuration, which belong to the cluster's own
repository and are **unassessed** here.

| # | Control area | Before | After | Basis |
|---|---|:---:|:---:|---|
| 5.1 | RBAC & Service Accounts | 1.0 | 1.0 | **read.** Namespaced Roles only — no ClusterRole, no ClusterRoleBinding, no `cluster-admin` anywhere in the tree, stated as a rule in `k8s/tak-operator/rbac.yaml`. The Hub's own Role is two leases verbs plus `secrets get/patch` narrowed by `resourceNames`. Caveat: `hub-verify` holds `pods/exec` into Hub pods, which is effectively equivalent to reading every Hub secret. |
| 5.2 | Pod Security Standards | 0.0 | 0.5 | **measured.** Was: container `securityContext` empty on hub/stunnel/basemap and absent entirely on nats/keydb/edge-relay; namespace audited at `baseline`, which says nothing about capabilities, privilege escalation, root filesystem or seccomp; `meshsat-hub-db` had no labels at all. Now hub, stunnel and basemap are `restricted`-compliant (verified by server-side dry-run: 9 violating workloads → 7), the namespace audits `restricted`, and the DB namespace is labelled. Not 1.0: no enforcement, and seven workloads still violate — two of them legitimately. |
| 5.3 | Network Policies & CNI | 0.0 | 0.5 | **measured.** Was: zero NetworkPolicies cluster-wide, while two manifests claimed one enforced tor-only access to port 6079. Now confined, each proven by a before/after connection test from an unrelated pod: the Hub's onion and Reticulum ports (tor-only, stunnel-only), all five NATS listeners including the unauthenticated cluster route port, and the **Postgres cluster** — 5432, 8000 and 9187 were all reachable from any pod. Still 0.5 and not 1.0: no default-deny anywhere, egress deliberately untouched (it carries the WAL archive), and keydb, stunnel, basemap, apprise, hawkbit, wg-easy and the edge relay remain uncovered. |
| 5.4 | Secrets Management | 1.0 | 1.0 | **read.** Every secret an `ExternalSecret` against the OpenBao `ClusterSecretStore`, `creationPolicy: Owner`, `deletionPolicy: Retain`. The one committed plain `Secret` carries an env-var placeholder, not a value. |
| 5.5 | Extensible Admission Control | 0.0 | 0.0 | **read.** No image-provenance or signature admission. Images are digest-pinned in `kustomization.yaml` — which is pinning, not verification — and three (`busybox`, `apprise`, `wg-easy`) resolve to `:latest`, including the initContainer that handles the onion private key as uid 0. |
| 5.7 | General Policies | 0.5 | 0.5 | **read.** No `ResourceQuota` or `LimitRange` on `meshsat-hub` (`meshsat-tak` has both). `automountServiceAccountToken` unset on hawkbit, wg-easy and apprise, so they run with the default SA token mounted. PDBs present on the five workloads that matter. |

**CIS ch. 5: 2.5 / 6 = 42% → 3.5 / 6 = 58%**

---

## Detection & response

Scored separately because ASVS V16 covers whether events are *recorded*, not whether anyone is
*told*.

| Capability | Before | After |
|---|---|---|
| Authentication failures | no metric, no log, no audit row | counter by reason and channel + `Warn` line with the real client IP |
| Authorisation denials | invisible | counter by requirement, separate series from 401s |
| Alert rules of any kind | **zero** in the repo and none security-related in the cluster | 12 rules in 3 groups, loaded, `health=ok` |
| Series existence | three counters alerts would target had **no series at all**, so those alerts could never fire | materialised at startup |
| Client IP in logs | the ingress pod's address on every line | resolved through the trusted-proxy walk |
| Break-glass token use | no metric, no log, no audit row — indistinguishable from the nightly job | counted, logged, audited per use; refused over Tor; alerted |
| Log-based alerting | Loki + promtail + a ruler exist; nothing wired, and `loki-0` is not ready | unchanged — still open |
| Audit hash chain | does not cover `created_at`/`id`/`tenant_id`; tolerates truncation at both ends; forks across the two replicas | unchanged — still open |

**Detection: ~10% → ~70%**

---

## Overall

| Instrument | Before | After |
|---|:---:|:---:|
| OWASP ASVS 5.0 L2 | 69% | **88%** |
| CIS Kubernetes ch. 5 | 42% | **58%** |
| Detection & response | ~10% | **~70%** |

The programme's targets were ASVS ≥ 95%, CIS ≥ 90% and detection ≥ 90%. **None is met**, and the
gap is not cosmetic. V6 is a half rather than a zero only because the static platform-admin bearer is
now monitored — it is still static and still non-expiring. Both CIS scores are capped at 0.5 because
nothing is *enforced*: the pod-security profile audits, and the network policies are per-workload
allow-lists rather than default-deny.

## Open, in the order they should be closed

1. **`HUB_AUTH_TOKEN`** — now monitored (MESHSAT-1195) but still static, non-expiring and rotated
   only by redeploy. Closing V6 means either an expiring platform credential, which requires letting
   an API key carry the platform flag, or removing the need for the platform axis from the tooling.
2. **PodSecurity enforcement** and the five third-party workloads; the tor initContainer running as
   uid 0 on an unpinned `busybox:latest` while handling the onion private key.
3. **Default-deny** network policy, plus egress. The highest-value targets are now confined
   individually — Hub non-public ports, every NATS listener, and Postgres — but a default-deny is
   what makes the *next* workload safe by default instead of by remembering.
4. **CI gates nothing** — `.gitlab-ci.yml` says so outright. `owasp:baseline` cannot fail
   (`allow_failure` + `|| true` + `-I`), never loads its own ruleset (`GIT_STRATEGY: none`), and runs
   unauthenticated against a Hub that 401s everything. No SBOM, no signing, no secret detection and no
   IaC scanning. **Fuzzing now exists** (MESHSAT-1202): a nightly `test:fuzz` job, seeded corpora
   running in the ordinary `test` job so a known-bad input blocks a push, and the unbounded-growth
   path in the HDLC reader that motivated it is fixed. One target so far — the parsers in
   `internal/codec`, `internal/fragment`, `internal/protocol` and `internal/wire` are still
   uncovered.
5. **Edge**: ingress-nginx ModSecurity is `DetectionOnly` and emitted zero audit records in 24 h; the
   VPS fail-closed WAF scope is `/auth/` and misses the Hub's actual `/api/auth/` login path.
6. **Audit chain** integrity and the missing events across the whole credential surface.
   Measured while scoping it 2026-09-17, because the obvious fix is a trap: `ComputeHash` covers
   `action|actor|detail|ip|prev_hash` only, so an entry can be moved between tenants or
   back-dated without breaking the chain. `tenant_id` is free to add (`Log` already takes it) and
   `id` is app-generated in `InsertAuditEntry`, but `created_at` is a database default and is not
   available at hash time. **The trap:** changing the formula invalidates every existing entry, so
   the natural remedy is for `VerifyChain` to fall back to the legacy formula — and that fallback
   is itself the bypass, since an attacker who edits a row can simply recompute it the legacy way
   and be accepted. A correct fix therefore needs a stored hash-version column, i.e. a migration,
   plus a database-side lock or a unique constraint on `(tenant_id, prev_hash)` for the separate
   cross-replica fork (`s.mu` is a process-local mutex and both replicas write). Not attempted
   half-way: a partially-covered digest that still reports "verified" is worse than a 0.5 that is
   written down.
7. ~~**MESHSAT-1032**~~ — **CLOSED.** The mechanism was removed by the move to per-tenant hosted
   TAK; this round added the tests that hold it and audited every other `DualFilters` consumer,
   which is where a second instance of the same shape would have been. Outstanding in
   `meshsat-website`: the privacy page has not been checked against what the Hub does with
   location data.
8. ~~**Scanner truth**~~ — **CLOSED 2026-09-17 (MESHSAT-1200)**, and the reality was worse than
   this entry stated. The scripts did point at the DMZ decommissioned on 2026-09-08, but they had
   not been *executed at all* since **2026-08-04**: `weekly-scan.sh` runs the extended sweep under
   `timeout 1800`, and phase E alone spends that budget (20 nikto hosts x `-maxtime 90s` = 1800 s
   exactly), so phases F, J, **H (mTLS)**, I and K were cut off every night on both scanners. The
   phase that reports "coverage UNKNOWN" cannot report its own absence, so the daily mail simply
   stopped mentioning mTLS and read clean. Fixed by moving phase H ahead of the enrichment phases
   (**assertions before enrichment**), raising the ceiling to 5400 s, repointing at notrf01, and
   probing **each A record separately** so a silently-dropping edge is named instead of averaging
   into weather one run in three. Both rigs now assert: NL 27 + 15 PASS, GR 22 + 16 PASS, zero FAIL,
   zero UNREACHABLE — **the first mTLS results grskg01sec01 has ever produced**. Inversion-tested
   both directions (`PUB_HOST=get.cubeos.app` -> 6 FAIL; `MQTT_PORT=443` -> 6 FAIL). A plaintext
   broker password hardcoded in a world-readable script was retired with the test that needed it.
9. ~~**`Onion-Location`**~~ — **CLOSED**. Served by all three VPS edges for `hub.meshsat.net`
   non-API paths, so a Tor Browser user is offered the hidden service automatically.

## What measurement caught

Three findings survived only because something was run rather than reasoned about, which is the
argument for the evidence standard at the top:

- **`nats:6222` looked contained.** Connecting through the `nats` Service fails — but only because
  that Service does not publish the port. The headless FQDN and the pod IP both worked.
- **Two manifests asserted a NetworkPolicy that did not exist.** `k8s/hub/service.yaml` and
  `k8s/hub/configmap.yaml` both stated that only the tor pod could reach 6079. An unrelated
  third-party pod reached it on the first try.
- **36 `govulncheck` advisories were a local artifact.** They came from the runner's Go 1.25.0; the
  deployed binary reports `go1.25.14` and CI's gate passes. Reported as a production finding, they
  would have been wrong.

And one the other way: the onion key rotation left `onion-heartbeat` probing a dead address, because
it reads the hostname once at pod start and carried no reloader annotation. The change had worked;
the monitor would have said it had not.
