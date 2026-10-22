package sqlite

import (
	"path/filepath"
	"testing"
)

// pragmas opens a store with DSN and reads back what SQLite runs with: the
// string form used to be ignored by modernc, so only a read-back proves it.
// [MESHSAT-1644]
func pragmas(t *testing.T) map[string]string {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "hub.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	got := map[string]string{}
	for _, p := range []string{"journal_mode", "busy_timeout", "foreign_keys", "synchronous"} {
		var v string
		if err := db.rawDB.QueryRow("PRAGMA " + p).Scan(&v); err != nil {
			t.Fatalf("PRAGMA %s: %v", p, err)
		}
		got[p] = v
	}
	return got
}

// Production runs WAL, synchronous NORMAL, a 5 s busy timeout and foreign
// keys off, as Postgres has none of this schema's three; the test-only fast mode (one variable only the CI jobs set)
// switches durability off and nothing else.
func TestDSNPragmasAreAppliedAndFastModeOnlyInTests(t *testing.T) {
	t.Setenv("HUB_SQLITE_TEST_FAST", "")
	want := map[string]string{"journal_mode": "wal", "busy_timeout": "5000", "foreign_keys": "0", "synchronous": "1"}
	if got := pragmas(t); !equal(got, want) {
		t.Fatalf("production pragmas %v, want %v", got, want)
	}
	t.Setenv("HUB_SQLITE_TEST_FAST", "1")
	want = map[string]string{"journal_mode": "memory", "busy_timeout": "5000", "foreign_keys": "0", "synchronous": "0"}
	if got := pragmas(t); !equal(got, want) {
		t.Fatalf("fast-mode pragmas %v, want %v", got, want)
	}
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
