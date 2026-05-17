# Requirements — Wire MO Fragment Reassembly (meshsat-hub v1.1, MESHSAT-168)

Source: `docs/ROADMAP.md` L74 ("MO fragment reassembly: Reassembler complete but SetReassembler() never called in main.go"), `docs/EXECUTION_PLAN.md` §"Task 1: Wire MO fragment reassembly (MESHSAT-168)" — P0 silent failure ("incoming multi-fragment SBD messages are silently dropped").

Constitution invariants in scope: Article II (security #1), Article III (httpjson.ReadJSON), Article VIII (audit log SHA-256 hash chain), Article IX (webhook authenticity verified BEFORE processing), Article XII (single-source-of-truth schema parity).

The `fragment` package (`internal/fragment/`) already implements `Reassembler` (declared at `fragment.go:110`, constructor `NewReassembler` at `fragment.go:117`). The RockBLOCK handler already exposes `SetReassembler` (declared at `rockblock/handler.go:114`). `main.go` never calls either — so fragmented MO payloads are passed downstream un-reassembled and silently lost.

## Functional requirements

REQ-100: The system shall instantiate `fragment.NewReassembler(5*time.Minute)` once during Hub startup in `cmd/meshsat-hub/main.go`.

REQ-101: The system shall call `rockblock.Handler.SetReassembler(reassembler)` after the RockBLOCK handler is constructed and BEFORE the HTTP router is started.

REQ-102: The system shall call `cloudloop.WebhookHandler.SetReassembler(reassembler)` against the same reassembler instance, so both ingress paths share one in-flight fragment table.

REQ-103: When the Hub starts, the system shall spawn a goroutine that calls `reassembler.Expire()` every 60 seconds to evict timed-out fragment groups.

REQ-104: When a MO message arrives whose payload header marks it as a fragment, the ingress handler shall call `reassembler.AddFragment(imei, rawBytes)` instead of publishing the partial payload to MQTT.

REQ-105: When `reassembler.AddFragment` returns reassembled bytes (non-nil), the ingress handler shall publish the reassembled bytes downstream using the same `mo/raw` + `mo/decoded` MQTT topics as a non-fragmented MO.

REQ-106: When `reassembler.AddFragment` returns `(nil, nil)` (more fragments expected), the ingress handler shall return early without publishing to MQTT and shall log `msg=fragment buffered, waiting for more` at INFO level.

REQ-107: When `reassembler.AddFragment` returns an error, the ingress handler shall write an audit entry of type `fragment.error` with the error string and shall NOT publish to MQTT.

REQ-108: When a fragment group times out and is evicted by `reassembler.Expire`, the system shall write an audit entry of type `fragment.timeout` containing `imei`, `message_id`, and `fragments_received`.

REQ-109: The system shall expose `GET /api/fragments/inflight` returning the current in-flight fragment groups with `imei`, `message_id`, `fragments_received`, `fragments_total`, `first_seen_at`.

REQ-110: When an operator queries `/api/fragments/inflight`, the system shall filter results by the operator's `tenant_id` JWT claim.

REQ-111: When the Hub shuts down gracefully, the system shall call `reassembler.Stop()` to halt the expiry goroutine and persist no fragment-table state (in-flight fragments are lost on restart — by design; carrier will retransmit).

REQ-112: The fragment-handling path shall increment the `hub_fragment_received_total` Prometheus counter labelled by `state` (`buffered`, `reassembled`, `error`, `timeout`).

REQ-113: When the same fragment (same `imei` + `message_id` + `fragment_index`) arrives twice, the reassembler shall treat the second arrival as a no-op and the system shall increment `hub_fragment_received_total{state="duplicate"}`.

REQ-114: When an integration test sends a 3-fragment MO message ordered (1, 2, 3), the system shall publish exactly one `mo/raw` + one `mo/decoded` MQTT message containing the concatenated payload.

REQ-115: When an integration test sends a 3-fragment MO message ordered (3, 1, 2) (out-of-order arrival), the system shall still publish the same single reassembled MQTT message.

REQ-116: When a fragment group reaches the 5-minute timeout without all fragments arriving, the system shall NOT publish a partial reassembly to MQTT.
