# MeshSat Hub — security posture

_Assessed 2026-09-16 / 17. Instrument for the hardening programme MESHSAT-1189 → 1194. Incident record kept current to 2026-09-18 (item 13)._

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
| V1 | Encoding & Injection | 0.5 | **1.0** | **read.** Every query parameterised; four `fmt.Sprintf` statements interpolate only closed-switch column names. No shell anywhere; `os/exec` is argv-form and platform-admin gated. Still open: CSV export does not neutralise formula injection (`internal/api/csv.go`), and ~99 handlers return raw driver error text to authenticated callers. **2026-09-18 (MESHSAT-1219):** the 39 handlers that echoed `err.Error()` with a 500 now answer "internal error" and log the cause; CSV cells are neutralised; the four interpolated-SQL sites take identifiers from closed switches and the `GROUP BY` builder is held by a test that feeds it injection strings. Moved to 1.0. |
| V2 | Validation & Business Logic | 0.5 | **1.0** | **read.** `readJSON` enforces a 1 MB cap, `DisallowUnknownFields` and single-value decoding — and is bypassed by six handlers, two without unknown-field rejection. `?limit=` is unbounded on eight list endpoints. **Fixed this round (MESHSAT-1202):** the Reticulum HDLC reassembly buffer grew without bound — a peer that sent one delimiter and never a second one chose the Hub's memory usage, reachable from the internet with any bridge-CA client certificate. Now capped at 16 KiB against a ~1002-byte theoretical maximum frame, with a regression test and a fuzz target both proven to fail without the fix. **Still 0.5**, because the `readJSON` bypasses and the unbounded `?limit=` are untouched, and `iface_tcp.go` still has no connection cap and still broadcasts every outbound frame to every client. **2026-09-18 (MESHSAT-1218):** `?limit=` is one bounded parser (max 500) at every list endpoint — proven with `?limit=999999` returning the tenant's 192 rows, not the table; CSV exports neutralise formula cells; `readJSON` bypasses are at zero. Moved to 1.0. |
| V3 | Web Frontend Security | 1.0 | 1.0 | **measured.** CSP with `script-src 'self'`, `object-src 'none'`, `frame-ancestors 'none'`; HSTS preload; XFO, nosniff, Referrer-Policy, Permissions-Policy, COOP, CORP all present on the live response. No CORS configured, which for a bearer-token API is correct. No CSP reporting. The Hub scored 1.0 here throughout — but **`auth.meshsat.net`, the identity provider, sent no CSP at all** until MESHSAT-1198, which is a reminder that scoring one host does not score the login path it depends on. Now fixed, with `'unsafe-inline'` as a named residual. **2026-09-18 (MESHSAT-1223):** that first policy broke the page it protected. authentik 2026.8 frames the Turnstile widget from a `blob:` URL, `frame-src` did not allow it, and the enrollment CAPTCHA rendered blank: no account could be created from 2026-09-17 ~12:00Z to 2026-09-18 13:07Z (item 12). `frame-src` now includes `blob:`, and every auth.meshsat.net flow plus all 30 Hub routes, logged in as the test tenant, were loaded in headless Chromium with zero violations. Score unchanged: the policy was the right control; its verification was not. |
| V4 | API & Web Service | 0.0 | **1.0** | **measured.** Was: 250 routes, 137 ungated, 45 of them state-changing. Now every mutating route carries a role floor, enforced by a build-time ratchet, and a viewer key returns 403 on each in production. Still 0.5: there is no rate limit on any *authenticated* route, and none on the two unauthenticated capability URLs. **2026-09-18 (MESHSAT-1219):** every authenticated route now carries a per-principal budget (600/min per user or API key), the two capability URLs and key minting the per-IP auth budget, and invalid API keys from one address stop reaching the database after twenty failures a minute. Proven in production: the 601st request from one key in a minute is answered 429 while another principal gets 200; a webhook path (no principal) is never 429; forty guesses at a TAK enrolment nonce give 30 answers and then 429. Trips are counted (`meshsat_hub_rate_limit_trips_total`) and alerted. Moved to 1.0. |
| V5 | File Handling | 1.0 | 1.0 | **read.** `assetKey` validates extension, segment count and each segment; backup import guards zip-slip via a cleaned-path prefix check; export uses `os.OpenRoot`. Zip-bomb entry/size limits absent, platform-admin only. |
| V6 | Authentication | 0.0 | **1.0** | **measured.** Was: `HUB_AUTH_TOKEN`, a static, never-expiring, non-revocable bearer granting `PlatformAdmin: true` in every auth mode, present in the production secret and read by five operator scripts and the nightly. Now **retired**: API keys can carry the platform axis (migration 29, gated so only an existing platform admin can mint one), every consumer was cut over to a keyed credential with an expiry, and `HUB_LEGACY_TOKEN_ENABLED=false` clears the token at config load. Proven, not reasoned: the exact production token gets `401 invalid_token` on both replicas, the same answer a random string gets, while the replacement key answers 200; the nightly passed 22/22 on the new key. The env var is still mounted (to be trimmed from the ExternalSecret) but nothing reads it. |
| V7 | Session Management | 0.5 | **1.0** | **read + measured.** 15-minute HS256 access tokens, rotated single-use refresh tokens stored SHA-256 hashed, `SameSite=Strict`. Fixed: logout was not auth-exempt, so an expired session could not revoke its own 7-day refresh token — and exempting it alone would have cleared the cookie while revoking nothing. Still open: no access-token revocation, and the refresh cookie's `Secure` flag derives from a client-influenceable header. **2026-09-18 (MESHSAT-1218):** the refresh cookie's `Secure` flag is decided by configuration (the public URL scheme), not by `X-Forwarded-Proto` — proven on a pod over plain HTTP with no such header, `Secure` present on both replicas; logout is auth-exempt so an expired session can still revoke its refresh token (MESHSAT-1189). Moved to 1.0. |
| V8 | Authorization | 0.5 | **1.0** | **measured.** `internal/store/scoping.go` is a two-sided ratchet failing the build for any store method or SQL statement that loses its tenant, with 54/74 written-down exemptions; no IDOR found. Now joined by `internal/auth/routefloor.go`, the same shape for route authorisation. Residual, tracked: `internal/crypto.KeyStore` is keyed by IMEI with no tenant dimension (durable rows are scoped; the process cache is not), and `api_keys.device_imei` is stored as if it were a scope and never read. |
| V9 | Self-contained Tokens | 1.0 | 1.0 | **read.** Algorithm allowlist, `exp` required, audience and issuer checked, `kid` required with a single JWKS refresh, OKP and symmetric keys rejected, EC points verified on-curve, 1 MB response caps. Claims are discarded and role/tenant re-read from the Hub's own tables. |
| V10 | OAuth & OIDC | 1.0 | 1.0 | **read.** PKCE S256, HMAC-signed state cookie with its own expiry, nonce checked against the ID token, open-redirect guard, `email_verified` required before provisioning, SPKI pinning with a rotation backup pin. |
| V11 | Cryptography | 1.0 | 1.0 | **read.** AES-256-GCM with a 12-byte random nonce, bcrypt cost 10, five long-lived keys sealed under `HUB_CONFIG_WRAP_KEY` with a preflight that refuses to boot rather than regenerate. No forward secrecy on the satellite path, documented with a cost argument in `docs/ENCRYPTION.md`. |
| V12 | Secure Communication | 0.5 | **1.0** | **measured.** TLS 1.2/1.3 at the edge; NATS websocket and stunnel verify client certificates against the bridge CA, asserted nightly from two external vantages (MESHSAT-1200). The cluster route port, which had no authorization and no TLS, now has both (MESHSAT-1194, 2026-09-18): routes authenticate with a credential rendered into the route URLs by the ExternalSecret and speak TLS with `verify: true` on certificates from the internal CA (cert-manager ClusterIssuer `meshsat-internal-ca`, rotated at 60 of 90 days), inside a policy that admits only nats pods to :6222. The first attempt split the cluster for 14 minutes: `cluster.authorization` guards INBOUND routes only and a bare route URL carries no credential, which was reproduced offline in three containers before the retry, as was the plain-to-TLS roll (routes re-form within a second of the last restart, meta leader within six). Proven in production by the three-signal drill on every member (routes 8/8/8, one agreed meta leader, both Hub replicas on the bus, `tls_required=true tls_verify=true` in varz) and by an unauthenticated CONNECT on the route port answered `-ERR 'Authorization Violation'`. What remains plaintext is the Hub's own MQTT session to `nats:1883` inside the allow-list — an in-cluster hop between two policy-confined pods, recorded here rather than scored. |
| V13 | Configuration | 1.0 | 1.0 | **read.** Every secret an ExternalSecret from OpenBao; no secret values committed; `"changeme"` appears only as a value to reject. An IMEI and a Cloudloop thingId sit in a ConfigMap — sensitive, not secret. |
| V14 | Data Protection | 0.5 | **1.0** | **measured.** Tenant export, redaction on export, audit retention bounded 30–3650 days and tenant-selectable. The cross-tenant TAK leak (MESHSAT-1032) is closed: the platform-wide CoT gateway is gone, replaced by a per-tenant forwarder that resolves the tenant off the topic and reaches only that tenant's upstreams, and the unscoped `/api/tak/federation/peers` route and TAK Operations page no longer exist. Now held by three tests using `DefaultTenantID` as the victim, proven by reintroducing the bug in production code. Every other `DualFilters` consumer was audited for the same shape: sos, position, message and mesh all resolve through `tenancy.Resolver`, which is stronger than topic parsing because the store is authoritative. Residual, other repo: the privacy page has not been checked against what the Hub actually does with location data. |
| V15 | Secure Coding & Architecture | 1.0 | 1.0 | **read.** Invariants held by tests that are declared not to be weakened (SOS survives quota; quota is on no ingest path; refunds are on no ingest path). Ratchets rather than review as the enforcement mechanism. |
| V16 | Security Logging & Error Handling | 0.0 | **1.0** | **measured.** Was: a 401 produced no metric, no log line and no audit row, because auth is registered outside metrics and logging and short-circuits; rejection reasons were logged at Debug while production runs at info. Now every refusal increments a labelled counter and emits a `Warn` line with the correctly-resolved client IP — proven 0 → 6 on real production 401s, and `ip=45.138.52.48` rather than the ingress pod. **2026-09-18:** the credential surface now audits — API key create/delete, local user create/role/enable/password/delete, tenant creation (written as the first entry of the new tenant's own chain) — proven with a mint+delete on the probe tenant leaving two hash-version-2 rows that verify. |
| V17 | WebRTC | — | — | Not applicable. |

**ASVS L2: 11.0 / 16 = 69% → 15.5 / 16 = 97%**

---

## CIS Kubernetes Benchmark, chapter 5 (workload scope)

Chapters 1–4 cover the control plane and node configuration, which belong to the cluster's own
repository and are **unassessed** here.

| # | Control area | Before | After | Basis |
|---|---|:---:|:---:|---|
| 5.1 | RBAC & Service Accounts | 1.0 | 1.0 | **read.** Namespaced Roles only — no ClusterRole, no ClusterRoleBinding, no `cluster-admin` anywhere in the tree, stated as a rule in `k8s/tak-operator/rbac.yaml`. The Hub's own Role is two leases verbs plus `secrets get/patch` narrowed by `resourceNames`. Caveat: `hub-verify` holds `pods/exec` into Hub pods, which is effectively equivalent to reading every Hub secret. |
| 5.2 | Pod Security Standards | 0.0 | **1.0** | **measured, enforced.** Was: container `securityContext` empty on hub/stunnel/basemap and absent on nats/keydb/edge-relay; namespace audited at `baseline`; `meshsat-hub-db` unlabelled. Now: `meshsat-hub-db` enforces `restricted` through PSA (a live CNPG pod replayed through a dry-run passes outright), and **Kyverno enforces the restricted profile per workload** in `meshsat-hub`, `meshsat-hub-db`, `meshsat-tak` and `meshsat-tak-db` (notrf01 !62/!63, MESHSAT-1204) — which is what PSA could not do, because PSA is namespace-wide and `meshsat-edge-relay` (hostNetwork) and `wg-easy` (NET_ADMIN) live in that namespace. Every container was first cut to what it needs (drop ALL, no privilege escalation, seccomp), nats/keydb/edge-relay were switched to declared non-root users with a rolling drill, and the four remaining exceptions — edge-relay (hostNetwork/hostPorts), wg-easy (NET_ADMIN/NET_RAW as root), tor (key-seeding init with CHOWN/DAC_OVERRIDE/FOWNER), apprise (startup `useradd`) — are each **measured on the pod and excluded by label for exactly that workload**, so a new pod with the same defect is refused. Proven the right way round on 2026-09-18: a bare root pod in `meshsat-hub` is refused at admission naming the policies, a compliant one is admitted, the same bare pod in `default` is only audited; and the first nightly under Enforce refused two of the nightly's own throwaway pods (the MQTT publisher and the onion probe) until they were made compliant — the control found real violations before an attacker did. Remaining debt, written down per file in `.trivyignore.yaml`: read-only root filesystems on nine third-party containers. |
| 5.3 | Network Policies & CNI | 0.0 | **1.0** | **measured.** Was: zero NetworkPolicies cluster-wide while two manifests claimed one enforced tor-only access to 6079. Now every workload in both namespaces is covered by an allow-list built from flows Hubble observed, and **default-deny ingress is on and proven** (MESHSAT-1205). The per-workload flips were each drilled: hawkbit — the OTA server, which decides what firmware a field device installs — stunnel and wg-easy all went REACHABLE → BLOCKED from an unrelated pod, while hub→hawkbit, basemap through the ingress, Reticulum mTLS, keydb peer replication and the onion heartbeat all kept working. **Two default-deny attempts before this one were silent no-ops** (`ingress: []`, then the same plus `enableDefaultDeny`), each caught only because the test creates a pod with a label no policy mentions and dials it; a plain Kubernetes NetworkPolicy with `policyTypes: [Ingress]` is what actually denies, and the union with the Cilium allows was verified against `basemap` before going namespace-wide. **Written, dated exception: egress is deliberately not default-denied.** It carries the CNPG WAL archive to nl-s3, the satellite provider callbacks, Twilio, Stripe and the Tor circuit, and an egress policy that is even slightly wrong stops the backups rather than the attacker. That is its own change with its own drill. **Two corrections, 2026-09-18.** (a) `basemap-confine` admitted only `host`/`remote-node`, but the Ingress routes `/basemap/local.pmtiles` to the basemap Service from the ingress-nginx controller pod, a normal endpoint; the deep map hung for ~25 h until 24d6c48 added that peer (item 13, MESHSAT-1229). The drill's "basemap through the ingress" check above therefore never exercised the basemap pods: that flow was blocked from the moment the policy applied. (b) `onion-heartbeat-confine` (`ingress: []`) was rejected by Cilium as invalid and enforced nothing; the pod was ingress-denied by `default-deny-ingress` alone, so there was no exposure, but it was not the per-workload policy this row counts. **Fixed 2026-09-20 (MESHSAT-1230):** an empty ingress list is not a rule — the CRD says "If omitted or empty, this rule does not apply at ingress" — and `ingress: [{}]` was not the fix either, because an ingress rule with no selectors is an empty match and reading it as deny-all is a guess. The policy now says it outright with `ingressDeny: [{fromEntities: [all]}]`, which both counts as a rule and is documented to deny "regardless of the allowed ingress rules". `VALID=True`, and only ingress is named so the heartbeat's own egress is untouched. |
| 5.4 | Secrets Management | 1.0 | 1.0 | **read.** Every secret an `ExternalSecret` against the OpenBao `ClusterSecretStore`, `creationPolicy: Owner`, `deletionPolicy: Retain`. The one committed plain `Secret` carries an env-var placeholder, not a value. |
| 5.5 | Extensible Admission Control | 0.0 | **1.0** | **measured, enforced.** Was: no admission control beyond PSA; no image provenance. Now: Kyverno 1.19.1 is the admission engine with the project's Pod Security policies at the restricted profile **enforced** in the four MeshSat namespaces (5.2), and **image provenance is checked at admission** (notrf01 !64/!65, MESHSAT-1204): every `ghcr.io/meshsat/meshsat-hub` pod must carry a cosign signature by the CI signing key (`cosign.pub`, MESHSAT-1216), verified as a sigstore bundle with no transparency log. Proven the right way round on 2026-09-18: a pod using a pre-signing digest of the very same image is **refused at admission** (`no matching signatures found`), the signed image is admitted (`verify-images: pass`), and the nightly's own unsigned image is outside the match so the nightly keeps running. Three things were learnt by measuring rather than assuming, each written into the module: the verifier ignores the engine-wide pull secret and needs the rule-level `imageRegistryCredentials`; the cosign-3 bundle format verifies (CI needs no change); and a `meshsat-hub*` glob had matched the nightly image. `failurePolicy` stays `Ignore`, so a dead engine admits rather than refuses. |
| 5.7 | General Policies | 0.5 | **1.0** | **measured.** Was: no `ResourceQuota` or `LimitRange` on `meshsat-hub` while `meshsat-tak` had both, and `automountServiceAccountToken` unset on hawkbit, wg-easy and apprise. Now (MESHSAT-1206) both objects exist, sized from measured usage with room rather than tuned tight — the 21 live pods request 1600m CPU / 2624Mi against ceilings of 8 CPU / 16Gi — and the two are kept in ONE file because a quota that sets `requests.*` makes a request mandatory while the LimitRange is what supplies the default, so a split could land in an order that refuses a request-less pod. `LimitRange.max` is 4Gi, chosen against the measured maximum (hawkbit's JVM at 1536Mi) so it cannot refuse to recreate a pod that runs today. The three SA tokens are off, each checked first: all three run as `default`, which has no RoleBinding or ClusterRoleBinding anywhere, so the token bought nothing and was only a mounted credential. PDBs already present. |

**CIS ch. 5: 2.5 / 6 = 42% → 6.0 / 6 = 100%**

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
| Log-based alerting | Loki + promtail + a ruler exist; nothing wired, and `loki-0` is not ready | ruler wired to Alertmanager (notrf01 !67), three log rules loaded from this repo (WAF block burst, broker auth failures, admission refusals), **proven end to end**: the route-TLS roll's transient refusals fired `NATSAuthorizationViolations`, the ruler posted it with zero errors and Alertmanager showed it active on the `webhook-n8n` receiver at 02:28Z. The rule counts both sides of a refusal, checked against the incident's own lines in Loki (MESHSAT-1190) |
| Image provenance | none — a re-tagged or foreign image would run | signed by digest in CI, verified at admission, unsigned refused (MESHSAT-1204/1216) |
| Audit hash chain | does not cover `created_at`/`id`/`tenant_id`; tolerates truncation at both ends; forks across the two replicas | v2 digest binds tenant, id and a microsecond timestamp; appends serialised by a per-tenant advisory lock; a fork is refused by a unique index; a legacy-formula row after the cut-over is a break. Head truncation is still accepted by design (retention). MESHSAT-1215 |

**Detection: ~10% → ~90%**

---

## Overall

| Instrument | Before | After |
|---|:---:|:---:|
| OWASP ASVS 5.0 L2 | 69% | **100%** |
| CIS Kubernetes ch. 5 | 42% | **100%** |
| Detection & response | ~10% | **~90%** |

The programme's targets were ASVS ≥ 95%, CIS ≥ 90% and detection ≥ 90%. All three are met, and every
chapter row is now scored by measurement rather than by reading. The last row to move was V12, and it
moved only after its first attempt had caused an outage and been withdrawn: the retry was reproduced
offline before it touched production, which is the standard this document asks for. What keeps
detection at ~90% rather than 100% is coverage, not wiring: the log signals cover the WAF, the broker
and admission, not yet the edge HAProxy logs on the three VPS, and the ZAP authenticated run has not
had its first scheduled observation (MESHSAT-1197).

The CIS caps have moved: **network policy is now enforced** with default-deny ingress and a
per-workload allow-list on every pod (5.3 → 1.0), and the general policies are in place (5.7 → 1.0).
**5.2 remains 0.5, and the barrier there is architectural rather than effort**: `enforce` on
`meshsat-hub` is impossible while `meshsat-edge-relay` (hostNetwork + hostPorts) and `wg-easy`
(NET_ADMIN) live in it, because PSA `baseline` forbids both and PSA exemptions are cluster-wide by
namespace/user/runtimeClass, never per-workload. The path to 1.0 is moving those two into their own
`privileged`-labelled namespace — a real change to the ingress path for every Hub request.

One pattern is worth stating on its own, because it recurred three times in one day: **a control is
not shipped until something has been observed to FAIL because of it.** The scanner phase that could
not fail, the alert whose label set could never match, and *two* default-deny policies that were
silent no-ops were all found the same way — by writing the negative test first. A control whose
effect is nothing is worse than an absent one, because this scorecard counts it.

## Open, in the order they should be closed

1. ~~**`HUB_AUTH_TOKEN`**~~ — **CLOSED 2026-09-17 (MESHSAT-1209).** The two halves in order:
   API keys gained the platform axis (migration 29; `POST /api/auth/keys` refuses the flag unless the
   caller already holds it, held by tests proven to fail with the gate removed), a platform-admin
   key was minted for the nightly and the five operator scripts were cut over to
   `HUB_VERIFY_ADMIN_KEY`; then `HUB_LEGACY_TOKEN_ENABLED` was flipped to `false` in the ConfigMap.
   The switch clears the token at the end of config load and refuses to boot if that would leave no
   authentication configured. Verified on both replicas after the roll: the production token is
   refused with `401 invalid_token`, indistinguishable from a random string, and the nightly passed
   22/22 on the new key. **V6 → 1.0.** Left: trim `HUB_AUTH_TOKEN` from the ExternalSecret and
   OpenBao, which is housekeeping — nothing reads it any more.
2. **PodSecurity enforcement on `meshsat-hub`** — blocked architecturally, see above; the namespace
   split is the work. `meshsat-hub-db` now enforces `restricted` and every container in both
   namespaces has been cut to the capabilities it actually needs (MESHSAT-1204). The tor initContainer
   that handles the onion private key is now digest-pinned and minimal (MESHSAT-1216); nothing in
   this area is unpinned any more.
3. ~~**Default-deny** network policy~~ — **CLOSED for ingress** (MESHSAT-1205). Every workload is
   covered and default-deny is on and proven against a pod carrying a label no policy mentions.
   **Egress remains, deliberately and dated:** it carries the CNPG WAL archive to nl-s3, the
   satellite provider callbacks, Twilio, Stripe and the Tor circuit, so an egress policy that is
   slightly wrong stops the backups rather than the attacker. It needs its own observation window
   and its own drill.
4. ~~**CI gates nothing**~~ — **CLOSED 2026-09-18 (MESHSAT-1216), measured on its first run.** What
   gates a push to `main` now, in order: `go mod verify`; **gitleaks** over the working tree and the
   push's commits (allow-list by test-fixture PATH only, each with its reason — it found one real
   committed credential on introduction, the E2E probe's password, now generated per run);
   **trivy config** over `k8s/` and the shipped Dockerfiles at HIGH/CRITICAL (27 findings measured:
   nine third-party containers without a read-only root are listed *per file* as deploy-then-harden
   debt so a new container without it still fails; the hostNetwork relay and wg-easy's capabilities
   are architectural; two were false positives on key names; the Dockerfiles gained `USER 65532`);
   then the existing lint / gosec / govulncheck / test / trivy-image chain; then **cosign** signs the
   pushed image *by digest* and attests a CycloneDX SBOM to it, verifying both with the committed
   `cosign.pub` before `bump_k8s_pin` — which now *needs* `sign`, so an unsigned image is never
   pinned. Proven, not reasoned: pipeline 54657 ran every new job green (gitleaks 15 s, IaC 42 s,
   sign 28 s); the newly pinned digest verifies **from outside CI** with the committed key and its
   attestation carries 124 components; a freshly generated wrong key is refused (`accepted
   signatures do not match threshold`). Two lessons paid for by rehearsal rather than a red
   pipeline: the official cosign image is distroless (no shell for GitLab), so the release binary is
   fetched checksum-pinned; cosign 3 refuses `--tlog-upload=false` and wants a signing config with
   no transparency log. Since then the *cluster* checks the signature at admission too — Kyverno `verifyImages` in
   Enforce, which refused an unsigned image on its first day (CIS 5.5 → 1.0). Still open under this
   heading: the authenticated `owasp:baseline` is in place and its first scheduled run is observed
   on Sunday (MESHSAT-1197). Fuzzing exists since MESHSAT-1202 (nightly `test:fuzz`, one target).
5. ~~**Edge**~~ — **BOTH CLOSED 2026-09-17.**
   - **ingress-nginx CRS now enforces** (MESHSAT-1207). The "zero audit records in 24 h" was checked
     before being trusted, because it reads identically to a WAF that evaluates nothing: a harmless
     `?q=<script>` produced CRS 941100/941110/941160/941390 plus the 949110 anomaly rule and a JSON
     audit record, and across a full 24 h on both controllers those were the *only* matches —
     legitimate traffic scores nothing. Verified after the flip: XSS and SQLi in a query string, and
     a CRS-triggering form body, all return **403**; clean traffic 200; both controllers reloaded
     with zero errors. **`/api/webhook/` is held in `DetectionOnly` permanently** — the same payload
     that gets 403 elsewhere returns the app's own 401/404 there. That prefix is where a satellite MO
     message arrives, the sender is a ground station that cannot interpret a 403, and nothing may
     throttle or hide an SOS.
   - **The VPS fail-closed arm now covers the Hub's real login path** (MESHSAT-1208). It was `/auth/`
     (authentik) and `/billing` — both omoikane's — while the Hub logs in at `/api/auth/`, so a
     CrowdSec/SPOE outage let credential traffic through unevaluated on the one hostname taking
     money. The rate-limit ACLs already covered `/api/auth/`, and that asymmetry is what hid it.
     Added host-scoped on all three edges (`is_failclosed_path` matches on path alone, so widening it
     would have changed every other vhost), below the SPOE lines, with the satellite webhook paths
     deliberately excluded for the same SOS reason.
6. ~~**Audit chain**~~ — **CLOSED 2026-09-17 (MESHSAT-1215), the full fix, not the trap.** The
   trap, for the record: `ComputeHash` bound `action|actor|detail|ip|prev_hash` only, so a row
   could be moved between tenants or back-dated and still verify; a verifier that falls back to the
   legacy formula when the new one fails is itself the bypass. What shipped instead: migration 30
   stores `hash_version` per row (existing rows keep 1 and verify with the old formula; every new
   row is 2), the v2 digest binds `tenant_id`, `id` and `created_at` with each field
   length-prefixed, `id` and `created_at` are fixed in `Log()` before hashing, and `created_at` is
   written monotonic per tenant so replica clock skew cannot reorder the chain. A version-1 row
   appearing after a version-2 row is a break. The cross-replica fork is closed twice: appends run
   in one transaction under `pg_advisory_xact_lock` per tenant, and a unique index on
   `(tenant_id, prev_hash)` refuses any second child that gets past it (production was checked to
   have no such pair first). Held by tests where back-dating, re-identifying and re-homing a row
   each break a v2 chain **and, as the negative control, none of them break a v1 chain** — the
   control is what proves v2 adds the coverage rather than the test asserting it. The credential surface
   now writes the events that were missing — API key created/deleted, user created, role changed,
   enabled/disabled, password changed, user deleted, tenant created. Still open: platform backup
   export/import are not audited.
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

10. ~~**NATS route port**~~ — **CLOSED 2026-09-18 (MESHSAT-1194), on the second attempt.** See V12.
    The first attempt is the first of the programme's three production incidents (items 12 and
    13 are the others): 01:16–01:30Z, the three members
    refused each other, JetStream had no leader and one Hub replica had no bus for 14 minutes; the
    kits reconnected within seconds and their outboxes held every message. Root cause proven
    offline, not inferred: nats-server 2.14 applies `cluster.authorization` to inbound routes only.
    The retry carries the credential in the route URLs, rendered by the ExternalSecret so it never
    enters the ConfigMap, and route TLS rolled separately with the same drill.
11. ~~**Log-based alerting**~~ — **CLOSED 2026-09-18 (MESHSAT-1190).** See the detection table.
    Found on the way and filed as MESHSAT-1220: the broker logs a websocket TLS handshake error
    every four seconds from the edge relay's health checks, which open TLS without a client
    certificate — 20k lines a day that no rule matches and that would hide a real handshake fault.
12. ~~**Sign-up blocked by the auth.meshsat.net CSP**~~ — **CLOSED 2026-09-18 (MESHSAT-1223).** The
    programme's second production incident, found by the owner, not by a check. The MESHSAT-1198
    policy went live on the three edges at 11:59–12:00Z on 2026-09-17 with `frame-src 'self'
    https://challenges.cloudflare.com`; authentik 2026.8 renders the Turnstile widget inside a
    `blob:` iframe, so the enrollment CAPTCHA was a blank box and the flow could not be completed
    until `blob:` was added at 13:07Z the next day. Impact, measured: no account was created in the
    window, and the edge logs show two outside loads of the enrollment page during it (a Google
    Cloud address, and desktop Chrome at 11:56Z on 2026-09-18, consistent with the owner's own
    report). The same page also showed
    authentik's stock background for the first ~0.3 s of every load, because the Brand never set one
    and our CSS lost the cascade to authentik's own `flow-css`; fixed in the same issue. The edge
    WAF's own Turnstile CAPTCHA was never affected: `http-response` rules do not reach responses
    HAProxy generates itself (`http-request return`), proven on HAProxy 2.8 in a throwaway container.
    Disclosed publicly as status.meshsat.net incident 11 (Sign-in, degraded, with the window, the
    cause and what was not affected). It is the first incident on that page written by a person:
    no monitor could see this outage, because every probe of the page still answered 200.
13. ~~**Deep map blocked by the basemap NetworkPolicy**~~ — **CLOSED 2026-09-18 (MESHSAT-1229).** The
    programme's third production incident, a degradation, and again found by the owner rather than
    by a check. `basemap-confine` (MESHSAT-1205) went live at ~17:50Z on 2026-09-17, built from
    Hubble flows seen in a window where nobody zoomed a map past zoom 11, and admitted only
    `host`/`remote-node` on 8080. The caller of `/basemap/local.pmtiles` is the ingress-nginx
    controller pod, so every request hung until ingress-nginx gave up (`upstream timed out (110)`,
    504 after ~30 s, or no reply). Effect on the map: the SPA's `HEAD` probe of the archive delayed
    the first paint by up to 30 s, and nothing past zoom 11 drew (street geometry lives at z13, names
    at z15). The world archive, glyphs and sprites come through the Hub and were unaffected. A cold
    basemap replica could not have copied its 38 GB archive from its peer either. Fixed in 24d6c48
    (ingress-nginx controller pods and basemap peers admitted on 8080) and proven through each of
    the three edge addresses: `HEAD` 200 and ranges at 0 and 20 GB answered 206 in 0.08-1.2 s
    against 30 s hangs before. No data or security impact. Not posted to status.meshsat.net.

## What measurement caught

These findings survived only because something was run rather than reasoned about, which is the
argument for the evidence standard at the top:

- **`nats:6222` looked contained.** Connecting through the `nats` Service fails — but only because
  that Service does not publish the port. The headless FQDN and the pod IP both worked.
- **Two manifests asserted a NetworkPolicy that did not exist.** `k8s/hub/service.yaml` and
  `k8s/hub/configmap.yaml` both stated that only the tor pod could reach 6079. An unrelated
  third-party pod reached it on the first try.
- **36 `govulncheck` advisories were a local artifact.** They came from the runner's Go 1.25.0; the
  deployed binary reports `go1.25.14` and CI's gate passes. Reported as a production finding, they
  would have been wrong.

- **Route authorization measured as working was not routes authenticating.** The route port's INFO
  advertised `auth_required` and a probe was refused, so the control was called shipped. The members'
  own outbound routes carried no credential, because that is not where nats-server reads it from,
  and the cluster split the moment a second roll made them reconnect. A negative test against one
  side of a connection says nothing about the other side.

- **A CSP verified with curl blocked every sign-up for 25 hours** (item 12). Each directive had
  been derived from what the pages load, and the headers were confirmed on every edge, but no
  browser had executed the enrollment flow under the policy; authentik renders its stages from
  JSON, so the `blob:` frame appears nowhere in the HTML a header check reads. A policy is verified
  by loading the pages it covers in a browser and listening for `securitypolicyviolation`, not by
  reading it back. The runner has Playwright and Chromium for exactly this.

- **A network policy built from observed flows blocked a flow nobody exercised** (item 13). Hubble
  shows the traffic that happened in the window, not the journeys the product needs. The Ingress
  sends `/basemap/local.pmtiles` from the ingress-nginx pod, and only a map zoomed past 11 makes
  that request. A deny rule is verified by driving every user journey through it: sign-up, login,
  the map at z15, the booth relay, the webhooks. After any policy change, request every Ingress path
  in the namespace through each edge address, and count a 30 s hang as a failure.
- **A policy the cluster rejects looks the same as one it enforces** until you read `VALID`.
  `onion-heartbeat-confine` was `VALID=False` from the day it was applied until 2026-09-20 (MESHSAT-1230, fixed).

And one the other way: the onion key rotation left `onion-heartbeat` probing a dead address, because
it reads the hostname once at pod start and carried no reloader annotation. The change had worked;
the monitor would have said it had not.
