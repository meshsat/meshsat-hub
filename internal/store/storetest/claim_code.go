package storetest

import (
	"context"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// testClaimCode pins the Ko-fi claim code's minting against a real database in
// both dialects. It has to be the database that decides: the usage endpoint is
// served by two replicas, and a read-then-write let both see an empty column,
// both mint, and the later write win -- leaving the earlier caller looking at a
// code nobody stored, on a page that tells them to put it in their Ko-fi
// message. That payment could never be matched (MESHSAT-1005).
func testClaimCode(t *testing.T, s store.Store) {
	ctx := context.Background()

	tn := &store.Tenant{ID: "cc-one", Slug: "cc-one", Name: "Minter", Plan: plans.Free, Status: store.TenantActive}
	if err := s.CreateTenant(ctx, tn); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Ten concurrent first readers, each with its own candidate. Every one of
	// them must be handed the same code, and it must be the stored one.
	const racers = 10
	got := make([]string, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got[i], errs[i] = s.EnsureClaimCode(ctx, tn.ID, candidateCode(i))
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	for i, c := range got {
		if c == "" {
			t.Fatalf("racer %d came away with no code", i)
		}
		if c != got[0] {
			t.Errorf("racer %d got %q, racer 0 got %q -- concurrent minters disagree", i, c, got[0])
		}
	}

	stored, err := s.GetTenant(ctx, tn.ID)
	if err != nil {
		t.Fatalf("re-read tenant: %v", err)
	}
	if stored.KofiClaimCode != got[0] {
		t.Errorf("callers were shown %q but the database holds %q", got[0], stored.KofiClaimCode)
	}

	// A later call never re-mints: the code a customer has already been told
	// to quote must not change under them.
	again, err := s.EnsureClaimCode(ctx, tn.ID, "NEVERUSD")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if again != got[0] {
		t.Errorf("a later call changed the code from %q to %q", got[0], again)
	}

	// An empty candidate is a caller bug, not something to write.
	if _, err := s.EnsureClaimCode(ctx, tn.ID, ""); err == nil {
		t.Error("an empty candidate was accepted")
	}
}

func candidateCode(i int) string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	out := make([]byte, 8)
	for j := range out {
		out[j] = alphabet[(i*7+j*3+j*j)%len(alphabet)]
	}
	return string(out)
}
