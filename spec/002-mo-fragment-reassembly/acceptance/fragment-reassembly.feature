Feature: MO Fragment Reassembly (meshsat-hub v1.1, MESHSAT-168)

  Parent epic: MESHSAT-168. P0 silent failure per docs/ROADMAP.md L74.

  Background:
    Given the Hub is running with the fragment reassembler wired into main.go
    And the RockBLOCK handler has the same reassembler instance attached
    And the Cloudloop handler has the same reassembler instance attached
    And the reassembler timeout is 5 minutes
    And the expiry goroutine runs every 60 seconds

  # REQ-104 + REQ-105 + REQ-114 — happy-path 3-fragment in-order
  Scenario: 3-fragment MO arriving in order is reassembled and published once
    When 3 fragments for imei=300258060902280 message_id=42 arrive in order (1,2,3)
    Then exactly one mo/raw MQTT message is published for imei=300258060902280
    And the published payload has was_reassembled=true
    And the published payload has fragments_count=3
    And exactly one mo/decoded MQTT message is published for imei=300258060902280

  # REQ-115 — out-of-order reassembly
  Scenario: 3-fragment MO arriving out of order is still reassembled
    When 3 fragments for imei=300258060902280 message_id=43 arrive in order (3,1,2)
    Then exactly one mo/raw MQTT message is published
    And the reassembled payload bytes equal fragment1 ++ fragment2 ++ fragment3 (concatenated in index order, NOT arrival order)

  # REQ-106 — partial arrival does not publish
  Scenario: Partial fragment group does not publish to MQTT
    When 2 of 3 fragments for imei=300258060902280 message_id=44 arrive
    Then no mo/raw MQTT message is published
    And a log entry at INFO level with msg="fragment buffered, waiting for more" exists

  # REQ-107 — fragment error writes audit + does not publish
  Scenario: Fragment with corrupted header writes fragment.error audit
    When a fragment for imei=300258060902280 with a malformed fragment header arrives
    Then no mo/raw MQTT message is published
    And an audit_log entry of type "fragment.error" exists for imei=300258060902280
    And the audit entry's error_detail describes the malformation

  # REQ-108 + REQ-116 — timeout evicts and does NOT publish partial
  Scenario: Fragment group times out after 5 minutes and is evicted without partial publish
    Given 2 of 3 fragments for imei=300258060902280 message_id=45 arrived 6 minutes ago
    When the expiry goroutine ticks
    Then no mo/raw MQTT message is published
    And an audit_log entry of type "fragment.timeout" exists for imei=300258060902280 message_id=45 with fragments_received=2 fragments_total=3

  # REQ-112 — Prometheus counters
  Scenario: Successful reassembly increments hub_fragment_received_total
    When a 3-fragment MO completes reassembly for imei=300258060902280
    Then the counter hub_fragment_received_total{state="buffered"} is incremented by 2
    And the counter hub_fragment_received_total{state="reassembled"} is incremented by 1

  # REQ-113 — duplicate fragment is no-op
  Scenario: Duplicate fragment arrival is detected and counted
    Given fragment index 1 of 3 for imei=300258060902280 message_id=46 has already arrived
    When fragment index 1 of 3 for imei=300258060902280 message_id=46 arrives again
    Then the in-flight fragment group state is unchanged
    And the counter hub_fragment_received_total{state="duplicate"} is incremented by 1

  # REQ-102 — shared reassembler across RockBLOCK and Cloudloop ingress
  Scenario: Fragments arriving across different webhook handlers converge on the same group
    When fragment 1 of 2 for imei=300258060902280 message_id=47 arrives via the RockBLOCK webhook
    And fragment 2 of 2 for imei=300258060902280 message_id=47 arrives via the Cloudloop webhook
    Then exactly one reassembled mo/raw message is published

  # REQ-109 + REQ-110 — inspection API + tenant filter
  Scenario: GET /api/fragments/inflight returns tenant-scoped groups
    Given the operator authenticates with JWT containing tenant_id="tenant-A"
    And 2 in-flight fragment groups exist for tenant_id="tenant-A"
    And 1 in-flight fragment group exists for tenant_id="tenant-B"
    When GET /api/fragments/inflight is called by the operator
    Then the response status is 200
    And the response body contains exactly 2 fragment groups
    And every group has tenant_id "tenant-A"
