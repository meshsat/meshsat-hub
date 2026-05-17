# ADR-0008 — Hub-as-internal-CA + HAProxy SNI passthrough for mTLS bridge authentication

* Status: Accepted — codified after the fact 2026-05-17. Already shipped in production at NL+GR DMZ.
* Date: Originally decided during the mTLS bridge-auth sprint (v1.7-track); recorded as ADR 2026-05-17 during the deep-spec audit.
* Deciders: `ufwtqkgz@meshsat.net`
* Source documents: `docs/deployment.md` L468–L520 (mTLS Bridge Authentication section), `README.md` L46

## Context

Bridges connect to Hub over MQTT-over-WebSocket from the public internet. Two security questions:

1. **Bridge authentication**: how does Hub know a connecting client is a legitimate bridge, not an impersonator?
2. **TLS termination locality**: should the edge (HAProxy on VPS) terminate TLS and proxy plaintext to backend NATS, or should it pass TLS through untouched all the way to NATS?

For (1), shared-secret username/password is weak in practice (passwords leak, get reused, must be rotated). Industry-standard answer for fleet device authentication is mTLS with a per-device client certificate.

For (2), terminating at the edge means the edge node has access to all bridge traffic in plaintext — a single edge compromise becomes a tenant-wide data exposure. Pass-through preserves end-to-end TLS but constrains what the edge can do (no L7 inspection, no header rewriting, no path-based routing).

## Decision

### Hub acts as an internal CA

- Hub maintains a per-tenant root CA (ECDSA P-256). Initialized on first start, persisted in the Hub database, wrapped via master-key envelope encryption.
- Bridge onboarding (`POST /api/bridges/{id}/certificate`) issues an ECDSA P-256 **client certificate** with:
  - CN = bridge_id
  - 90-day validity (renewal expected before expiry; same model used in `meshsat/internal/pair/` per ADR-0006 of the bridge repo)
  - Signed by Hub's root CA
- The issued certificate + private key are returned to the operator ONCE on creation; never re-derivable.
- Hub auto-exports the bridge CA certificate to a shared `nats-certs` Docker volume (`HUB_BRIDGE_CA_CERT_EXPORT_PATH`) so NATS can be configured to trust it for client-cert verification.

### NATS verifies client certs

NATS is configured with TLS WebSocket on `:9443` and `verify: true`:

```
websocket {
  port: 9443
  tls {
    cert_file: /etc/nats/certs/server.crt    # your domain cert (NOT Hub-issued)
    key_file: /etc/nats/certs/server.key
    ca_file: /etc/nats/certs/bridge-ca.crt   # Hub CA (auto-exported)
    verify: true                              # require + verify client cert
  }
}
```

A bridge's TLS handshake to NATS:
1. NATS presents server cert (matching its domain, signed by Let's Encrypt or operator's CA).
2. NATS requests a client cert; bridge sends its Hub-issued cert.
3. NATS validates the client cert against the Hub CA bundle in `bridge-ca.crt`.
4. NATS extracts the CN (bridge_id) and uses it for per-bridge ACLs.

### HAProxy at the edge is SNI-passthrough (not TLS-terminating)

The VPS / edge HAProxy operates at L4 with SNI peeking — it reads the SNI value from the unencrypted TLS ClientHello, picks the backend by SNI, and TCP-passes the entire TLS stream through to the backend. **It does not have the server private key**; it cannot decrypt the stream; it cannot inspect MQTT topics or rewrite WebSocket headers.

```
Bridge (field device)
  |
  | wss://mqtt.example.com:443/mqtt  (client cert + key in TLS handshake)
  v
HAProxy (VPS edge, port 443)
  | SNI peek: server_name = mqtt.example.com  →  TCP passthrough to backend
  v
NATS (:9443, TLS + verify: true)
  | TLS handshake: server cert (*.example.com) + client cert verification
  | Client cert CN = bridge_id, signed by Hub CA
  v
MQTT-over-WebSocket session established
```

Measured round-trip: 266–345 ms end-to-end on the production NL+GR pair (`README.md` L46).

## Consequences

**Positive**
- mTLS chain is end-to-end. An edge compromise (HAProxy host stolen) does NOT expose bridge traffic in plaintext.
- Per-bridge revocation is a Hub-side operation — revoke the cert in Hub's CA registry, push updated CRL or rely on cert expiry. No edge changes needed.
- 90-day cert expiry forces operators into a rotation cadence rather than letting old certs accumulate forever.
- Edge is fully stateless w.r.t. bridge identity — adding edge capacity is `apt install haproxy` + DNS update.

**Negative**
- Operator can't use HAProxy for L7 features (path-based routing, header injection, OWASP rule sets, request rate-limiting). All L7 enforcement is Hub-side.
- DNS-based SNI is the only routing signal — the edge can't differentiate "this WSS is for Hub-NL" vs "this WSS is for Hub-GR" except via SNI hostname. If both nodes share `mqtt.example.com`, the edge selects backend via HAProxy's `balance roundrobin` or active health checks; bridges must tolerate either backend.
- mTLS handshake adds ~50–100 ms on top of plain TLS — accepted as a security-vs-latency tradeoff (Constitution Article II).
- Cert renewal is operator-initiated for now; auto-renewal is a future improvement.

## Alternatives considered

- **Edge terminates TLS, talks plaintext MQTT to NATS**: rejected — single edge compromise becomes total bridge-fleet exposure. Article II (security #1) forbids it.
- **No mTLS, just MQTT username/password**: rejected — username/password leaks, never rotates in practice, no per-bridge revocation without backend coordination. The `auto-provisioning` story (Hub generates bridge MQTT credentials per `README.md` L42–L43) keeps username/password as a fallback for legacy bridges, NOT as the recommended path.
- **External CA (Let's Encrypt for client certs)**: rejected — Let's Encrypt issues server certs, not arbitrary client certs; using a public CA for internal device identity would require a separate per-device DV process every 90 days.
- **WireGuard tunnel + plain MQTT inside**: rejected for the bridge ⇄ Hub edge path — adds a second crypto layer and another bootstrap step on each bridge; WireGuard IS used for separate use cases (`internal/wireguard/` peer management for inter-node infra traffic, not for bridge-edge authentication).

## Compliance

- Hub MUST keep its CA root private key wrapped via the master-key envelope (same operational contract as bridge's keystore — see meshsat repo Constitution Article XIII).
- Bridge cert issuance (`POST /api/bridges/{id}/certificate`) MUST refuse to issue without operator authentication (per Constitution Article IV — every Store method takes tenant_id; certs are tenant-scoped).
- HAProxy config (`deploy/haproxy/haproxy.cfg.example`) MUST stay SNI-passthrough only — NO `ssl crt` directive on the bind line. CI check or pre-deploy audit recommended (currently not automated; tracked for follow-up).
- NATS config MUST have `verify: true` set — NOT `verify_and_map: true` (which would also accept self-signed peers).
- The shared `nats-certs` Docker volume MUST be readable by both Hub and NATS containers; Hub writes the CA, NATS reads it on startup.
