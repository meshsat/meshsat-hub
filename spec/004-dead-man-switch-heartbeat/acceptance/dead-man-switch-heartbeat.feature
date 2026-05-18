Feature: Dead man switch heartbeat (spec/004)

  # Covers: REQ-300, REQ-301, REQ-302, REQ-303, REQ-304, REQ-305, REQ-306, REQ-307, REQ-308, REQ-309

  Scenario: Dead man switch heartbeat capability 1
    When REQ-300 is exercised
    Then the behaviour matches the spec

  Scenario: Dead man switch heartbeat capability 2
    When REQ-301 is exercised
    Then the behaviour matches the spec

  Scenario: Dead man switch heartbeat capability 3
    When REQ-302 is exercised
    Then the behaviour matches the spec

  Scenario: Dead man switch heartbeat capability 4
    When REQ-303 is exercised
    Then the behaviour matches the spec

  Scenario: Dead man switch heartbeat capability 5
    When REQ-304 is exercised
    Then the behaviour matches the spec

