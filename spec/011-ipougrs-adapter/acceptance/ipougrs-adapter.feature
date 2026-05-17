Feature: IPoUGRS Adapter (meshsat-hub v2.0 alpha, MESHSAT-183)

  Parent epic: MESHSAT-183. Experimental v2.0 alpha per docs/ROADMAP.md L150.

  Background:
    Given the Hub is built with the internal/ipougrs/ package

  # REQ-1000 — round-trip codec
  Scenario: Encode then decode round-trips the IP packet
    Given an IPv4 packet of 100 bytes
    When ipougrs.Encode(packet) returns encoded
    And ipougrs.Decode(encoded) returns decoded
    Then decoded equals the original packet

  # REQ-1001 — MTU enforced
  Scenario: Encode rejects packets exceeding MTU
    Given an IPv4 packet of 400 bytes
    When ipougrs.Encode(packet) is called
    Then the function returns an error describing the MTU exceeded

  # REQ-1002 — ingress hook on magic byte
  Scenario: MO with magic 0x49 routes to TUN
    Given HUB_IPOUGRS_ENABLED=true AND the TUN interface meshsat-ipougrs0 exists
    When an MO arrives whose first byte is 0x49 followed by version 0x01 and a valid encoded IPv4 packet
    Then the decoded IPv4 packet is written to the TUN interface
    And hub_ipougrs_encoded_total{direction="in"} is incremented by 1

  # REQ-1003 + REQ-1004 — graceful degradation when CAP_NET_ADMIN missing
  Scenario: Missing capability does NOT crash the Hub
    Given HUB_IPOUGRS_ENABLED=true AND the host lacks CAP_NET_ADMIN
    When the Hub starts
    Then the Hub does NOT crash
    And a log entry at ERROR level mentions CAP_NET_ADMIN is missing
    And GET /api/ipougrs/status returns enabled=false with a startup_error field

  # REQ-1006 — send via Cloudloop MT
  Scenario: POST /api/ipougrs/send encodes + delegates to Cloudloop MT
    Given HUB_IPOUGRS_ENABLED=true AND the Cloudloop MT client is configured
    When POST /api/ipougrs/send is called with destination_imei="300258060902280" and payload=base64(valid_100_byte_ipv4_packet)
    Then the response status is 202
    And the Cloudloop MT client was invoked with destination_imei="300258060902280"
    And the bytes sent begin with 0x49 0x01 (magic + version)

  # REQ-1007 — oversized payload rejected
  Scenario: Oversized payload returns 400
    When POST /api/ipougrs/send is called with payload=base64(500_byte_packet)
    Then the response status is 400
    And the response body field "error" mentions the MTU

  # REQ-1011 — cluster-mode opt-in requires explicit env
  Scenario: Cluster mode without HUB_IPOUGRS_ENABLED returns 503
    Given the Hub is running in cluster mode without HUB_IPOUGRS_ENABLED
    When POST /api/ipougrs/send is called
    Then the response status is 503

  # REQ-1012 — unknown version rejected
  Scenario: Unknown wire format version is rejected
    When an MO arrives with first byte 0x49 followed by version 0xFF
    Then the decoded IP packet is NOT written to the TUN
    And hub_ipougrs_decode_error_total{reason="unknown-version"} is incremented by 1
