# Requirements — IPoUGRS Adapter (meshsat-hub v2.0 alpha, MESHSAT-183)

Source: `docs/ROADMAP.md` L150 ("IPoUGRS adapter (IP-over-satellite). Experimental differentiator, not started"), `docs/EXECUTION_PLAN.md` §"Task 17: IPoUGRS adapter (MESHSAT-183)". Marked **alpha** in v2.0 — experimental feature, not on the v1.x critical path.

Constitution invariants in scope: Article II (security #1), Article XII (sqlite+mariadb parity), Article XVII (Sparkplug B wire-protocol invariants).

IPoUGRS = IP-over-Underground-Ring-Signal (or IP-over-Iridium-via-GSM-Ring-Signal — same idea, several spellings in the wild). Carries thin IP-shaped traffic over the Iridium SBD/IMT bearer by encoding into payload bytes. This adapter ingests IPoUGRS-encoded MO payloads and emits the inner IP packet to a local TUN interface for downstream IP-aware tooling.

## Functional requirements

REQ-1000: The system shall implement `internal/ipougrs/` package providing `Encode(ipPacket []byte) (encodedPayload []byte, err error)` and `Decode(encodedPayload []byte) (ipPacket []byte, err error)` codec functions.

REQ-1001: The IPoUGRS encoded payload shall fit within 340 bytes (the Iridium SBD MO maximum), accepting inner IP packets up to a tunable MTU (default 300 bytes after encoding overhead).

REQ-1002: When the RockBLOCK ingress handler decodes an MO whose first byte equals the IPoUGRS magic byte `0x49`, the handler shall call `ipougrs.Decode(payload[1:])` and route the resulting IP packet to a local TUN interface named `meshsat-ipougrs0`.

REQ-1003: The system shall create the TUN interface `meshsat-ipougrs0` at Hub startup ONLY when `HUB_IPOUGRS_ENABLED=true` is set, defaulting to disabled.

REQ-1004: When `HUB_IPOUGRS_ENABLED=true` AND the host lacks `CAP_NET_ADMIN`, the system shall log an ERROR at startup AND shall NOT crash; the IPoUGRS path shall be inert in this configuration.

REQ-1005: The system shall expose `GET /api/ipougrs/status` returning the TUN interface state, encoded/decoded packet counts, and any startup error.

REQ-1006: The system shall expose `POST /api/ipougrs/send` accepting `{destination_imei: string, payload: base64-encoded-ip-packet}` and shall encode + send via the existing Cloudloop MT delivery path.

REQ-1007: When `POST /api/ipougrs/send` is called with a payload exceeding the configured MTU, the system shall return 400 with an error describing the MTU.

REQ-1008: The system shall increment `hub_ipougrs_encoded_total{direction="in|out"}` and `hub_ipougrs_decode_error_total{reason="..."}` Prometheus counters.

REQ-1009: When `IPoUGRS_DEBUG_PCAP` env var is set to a path, the system shall write a pcap-format capture of every encoded + decoded IP packet for debugging.

REQ-1010: The feature shall be documented as **alpha** in the Hub OpenAPI spec via a `deprecated: false` + `x-stability: alpha` extension on every IPoUGRS endpoint.

REQ-1011: The system shall NOT enable IPoUGRS in cluster mode without explicit `HUB_IPOUGRS_ENABLED=true` AND a startup-time WARN log entry.

REQ-1012: The codec shall be wire-format-versioned: the IPoUGRS magic byte `0x49` is followed by a 1-byte format version (currently `0x01`); receivers shall reject payloads with unknown versions and increment `hub_ipougrs_decode_error_total{reason="unknown-version"}`.
