package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Twenty appenders on one tenant, racing. Every one must land, in one line,
// each linked to exactly the row before it: that is the advisory lock doing
// what a process mutex never could across two replicas. Without the lock the
// unique index turns the race into errors; with it there are none.
func TestAppendAuditEntry_SerialisesConcurrentAppenders(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tid := "racing-tenant"
	const n = 20

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- db.AppendAuditEntry(ctx, tid, func(prev *store.AuditEntry) (*store.AuditEntry, error) {
				prevHash := ""
				now := time.Now().UTC().Truncate(time.Microsecond)
				if prev != nil {
					prevHash = prev.Hash
					if !now.After(prev.CreatedAt) {
						now = prev.CreatedAt.Add(time.Microsecond)
					}
				}
				sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", i, prevHash)))
				return &store.AuditEntry{
					ID: fmt.Sprintf("aud-race-%d", i), Action: "race", Actor: "t",
					PrevHash: prevHash, Hash: hex.EncodeToString(sum[:]), HashVersion: 2, CreatedAt: now,
				}, nil
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("append under contention: %v", err)
		}
	}

	entries, err := db.ListAuditEntries(ctx, tid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("got %d rows, want %d", len(entries), n)
	}
	// newest-first: each row's prev_hash is the hash of the row after it.
	for i := 0; i+1 < len(entries); i++ {
		if entries[i].PrevHash != entries[i+1].Hash {
			t.Fatalf("row %s links to %q, not to its predecessor %s", entries[i].ID, entries[i].PrevHash, entries[i+1].ID)
		}
		if !entries[i].CreatedAt.After(entries[i+1].CreatedAt) {
			t.Fatalf("created_at not monotonic between %s and %s", entries[i+1].ID, entries[i].ID)
		}
	}
	if entries[len(entries)-1].PrevHash != "" {
		t.Fatalf("the first row should have an empty prev_hash, got %q", entries[len(entries)-1].PrevHash)
	}
}

// The index is the backstop for anything that bypasses AppendAuditEntry.
func TestInsertAuditEntry_ForkIsRefused(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tid := "forked"
	if err := db.InsertAuditEntry(ctx, tid, &store.AuditEntry{Action: "root", Hash: "r", HashVersion: 2}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertAuditEntry(ctx, tid, &store.AuditEntry{Action: "a", PrevHash: "r", Hash: "a", HashVersion: 2}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertAuditEntry(ctx, tid, &store.AuditEntry{Action: "b", PrevHash: "r", Hash: "b", HashVersion: 2}); err == nil {
		t.Fatal("second child of one parent accepted")
	}
	// Microseconds round-trip exactly; a v2 digest depends on it.
	got, err := db.GetLatestAuditEntry(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.Nanosecond()%1000 != 0 || got.CreatedAt.IsZero() {
		t.Fatalf("created_at %v is not a microsecond value the digest can reproduce", got.CreatedAt)
	}
}
