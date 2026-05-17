Feature: SMS + Email as Routing Destinations (meshsat-hub v1.3, MESHSAT-181)

  Parent epic: MESHSAT-181. Depends on spec/006 routing engine + v1.2 sms/email gateways.

  Background:
    Given the Hub is running with the routing.Engine started
    And the sms.Gateway and email.Gateway are configured
    And the dispatch.Registry has sms and email handlers registered

  # REQ-800 + REQ-802 — sms dispatch via device contact
  Scenario: Route to sms with device-contact lookup
    Given a route exists: tenant=tenant-A, source=mo_decoded, destination=sms, enabled=true, destination_address=""
    And device imei=300258060902280 has contact phone="+1-555-0123" in its contact list
    When an MO decoded message arrives from imei=300258060902280 for tenant-A
    Then sms.Gateway.Send is invoked with destination="+1-555-0123" and the message body

  # REQ-805 — sms dispatch with explicit address override
  Scenario: Route's destination_address overrides device contact
    Given a route exists: destination=sms, destination_address="+1-555-9999"
    And the source device has phone="+1-555-0123" in its contact list
    When a matching message arrives
    Then sms.Gateway.Send is invoked with destination="+1-555-9999" (not 0123)

  # REQ-801 + REQ-803 — email dispatch with subject
  Scenario: Route to email constructs subject and dispatches
    Given a route exists: source=sos, destination=email, destination_address="ops@example.com"
    When an SOS message arrives from imei=300258060902280
    Then email.Gateway.Send is invoked with destination="ops@example.com"
    And the subject contains "MeshSat: sos from 300258060902280"

  # REQ-809 — PGP encryption when key exists
  Scenario: Email handler PGP-encrypts when recipient has a key
    Given the email keystore has a PGP key for "secure@example.com"
    And a route exists: destination=email, destination_address="secure@example.com"
    When a matching message arrives
    Then email.Gateway.Send is invoked with a body that begins with "-----BEGIN PGP MESSAGE-----"

  # REQ-810 — no-address increments counter and skips dispatch
  Scenario: No address resolvable means counter increment, no send
    Given a route exists: destination=sms, destination_address=""
    And the source device has NO phone in its contact list
    When a matching message arrives
    Then sms.Gateway.Send is NOT invoked
    And hub_route_dispatch_error_total{reason="no-address", destination_type="sms"} is incremented by 1

  # REQ-807 — gateway error counter
  Scenario: SMS gateway error increments handler-error counter
    Given a route exists: destination=sms, destination_address="+1-555-0123"
    And sms.Gateway.Send will return an error on next call (Twilio 4xx)
    When a matching message arrives
    Then hub_route_dispatch_error_total{reason="handler-error", destination_type="sms"} is incremented by 1

  # REQ-808 — email gateway error counter
  Scenario: Email gateway error increments handler-error counter
    Given a route exists: destination=email, destination_address="ops@example.com"
    And email.Gateway.Send will return an error on next call (SMTP timeout)
    When a matching message arrives
    Then hub_route_dispatch_error_total{reason="handler-error", destination_type="email"} is incremented by 1

  # REQ-812 — POST/PUT validation when address is required
  Scenario: POST /api/routes with destination=sms but no address rejects
    Given the source device has no phone contact
    When POST /api/routes is called with body {destination_type="sms", destination_address=""}
    Then the response status is 400
    And the response body field "error" describes the missing address requirement
