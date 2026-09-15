package sqlite

import (
	"strings"
	"testing"
)

// The test-only fast mode must never leak into a production DSN: it is
// keyed on one environment variable that only the CI jobs set.
func TestDSNIsDurableUnlessTheTestFlagIsSet(t *testing.T) {
	t.Setenv("HUB_SQLITE_TEST_FAST", "")
	if d := DSN("/tmp/x.db"); !strings.Contains(d, "_synchronous=NORMAL") || !strings.Contains(d, "_journal_mode=WAL") {
		t.Fatalf("production DSN lost durability: %s", d)
	}
	t.Setenv("HUB_SQLITE_TEST_FAST", "1")
	if d := DSN("/tmp/x.db"); !strings.Contains(d, "_synchronous=OFF") || !strings.Contains(d, "_journal_mode=MEMORY") {
		t.Fatalf("fast mode did not apply: %s", d)
	}
}
