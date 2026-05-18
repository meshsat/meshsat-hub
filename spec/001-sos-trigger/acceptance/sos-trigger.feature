Feature: SOS trigger logic (spec/001 — RETROSPECTIVE)

  # Covers: REQ-001, REQ-002, REQ-003, REQ-004, REQ-005, REQ-006, REQ-007, REQ-008, REQ-009, REQ-010, REQ-011, REQ-012, REQ-013, REQ-014, REQ-015, REQ-016, REQ-017, REQ-018, REQ-019, REQ-020

  Background:
    Given a Detector is constructed with bus, engine, store, tenantID="t-acme", chainID="chain-sos"
    And the Detector has subscribed to meshsat/+/mo/decoded

  # REQ-002 + REQ-003 — keyword detection
  Scenario: Keyword "SOS" triggers emergency
    Given a MO decoded message arrives on meshsat/IMEI-XXX/mo/decoded with text "HELP SOS NOW"
    When the Detector processes the message
    Then an SOSEvent is published to meshsat/IMEI-XXX/sos
    And the SOSEvent has Source="keyword" and Keyword="SOS"

  Scenario: Keyword "MAYDAY" case-insensitive
    Given a message with text "mayday mayday"
    When processed
    Then an SOSEvent is published with Keyword="MAYDAY"

  Scenario: Keyword "EMERGENCY" embedded in longer text
    Given a message with text "Reporting an emergency at lat..."
    When processed
    Then an SOSEvent is published with Keyword="EMERGENCY"

  # REQ-004 — explicit field detection
  Scenario: Explicit sos:true field triggers emergency
    Given a message with text "battery low" and sos=true
    When processed
    Then an SOSEvent is published with Source="field"

  # REQ-005 + REQ-006 — payload shape
  Scenario: SOSEvent payload includes required fields
    When any SOSEvent is published
    Then the JSON includes imei, text, source
    And keyword is present when source="keyword"
    And source value is one of {"keyword", "field"}

  # REQ-008 + REQ-009 — escalation
  Scenario: Detected SOS fires the escalation engine
    Given chainID="chain-sos" is configured
    When an SOSEvent is published
    Then escalation.Engine.Fire is invoked with tenantID="t-acme" and chainID="chain-sos"

  # REQ-010 — empty chainID fallback
  Scenario: Empty chainID uses store's first available chain
    Given the Detector was constructed with chainID=""
    And the store has chains [chain-A, chain-B]
    When an SOSEvent fires escalation
    Then chain-A is used

  # REQ-011 — escalation failure tolerance
  Scenario: Escalation engine failure is logged + skipped
    Given escalation.Engine.Fire returns error
    When the Detector processes an SOS-triggering message
    Then slog.Error is invoked with the error
    And the Detector continues processing subsequent messages

  # REQ-014 + REQ-015 — fail-OPEN (Article VII)
  Scenario: Bus unavailable at startup logs + retries
    Given bus.MessageBus.Subscribe returns error at NewDetector time
    When the Detector starts
    Then a slog.Warn is logged
    And the Detector does NOT panic
    And a retry is scheduled

  # REQ-012 + REQ-013 — tenant isolation
  Scenario: All operations carry tenantID
    Given tenantID="t-acme" was passed to NewDetector
    When any escalation or store call is made
    Then tenantID="t-acme" is the first argument

  # REQ-016 + REQ-017 — keywords table-driven
  Scenario: Adding a 4th keyword requires editing sosKeywords var
    When inspecting internal/sos/detector.go
    Then sosKeywords is a package-level []string
    And keyword matching uses strings.Contains over uppercased text

  # REQ-020 — test coverage
  Scenario: detector_test.go covers all paths
    When `go test -v ./internal/sos/` runs
    Then tests pass for: 3 keywords times 2 case-variants, explicit field, no-SOS pass-through, escalation fire, chainID resolution
