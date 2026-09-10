package storetest

import (
	"context"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testOIDCClaim pins who wins when two sign-ins provision one new subject at
// the same time. LinkOIDCIdentity upserts, which is right for a returning user
// and wrong for a new one: both callbacks provision a tenant and the second
// upsert repoints the subject at its own, leaving the first fully populated,
// owned, counted by billing and reachable by nobody. Exactly one claim may
// succeed, and every loser has to be told which identity won (MESHSAT-1006).
func testOIDCClaim(t *testing.T, s store.Store) {
	ctx := context.Background()
	const issuer, subject = "https://auth.example/", "sub-race-1"

	const racers = 8
	type outcome struct {
		id      *store.OIDCIdentity
		claimed bool
		err     error
	}
	out := make([]outcome, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id, claimed, err := s.ClaimOIDCIdentity(ctx, &store.OIDCIdentity{
				Issuer: issuer, Subject: subject,
				UserID:   candidateCode(i),
				TenantID: "t-" + candidateCode(i),
				Email:    "racer@example.com",
			})
			out[i] = outcome{id, claimed, err}
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	var winner *store.OIDCIdentity
	for i, o := range out {
		if o.err != nil {
			t.Fatalf("racer %d: %v", i, o.err)
		}
		if o.claimed {
			winners++
			winner = o.id
		}
		if o.id == nil {
			t.Fatalf("racer %d got no identity back", i)
		}
	}
	if winners != 1 {
		t.Fatalf("%d racers claimed the subject; exactly one may", winners)
	}

	// Every loser must be handed the winner's identity, not its own: that is
	// what lets it discard the tenant it just built and adopt the right one.
	for i, o := range out {
		if o.claimed {
			continue
		}
		if o.id.TenantID != winner.TenantID || o.id.UserID != winner.UserID {
			t.Errorf("loser %d was told tenant %q/user %q, winner holds %q/%q",
				i, o.id.TenantID, o.id.UserID, winner.TenantID, winner.UserID)
		}
	}

	// And the stored row is the winner's.
	stored, err := s.GetOIDCIdentity(ctx, issuer, subject)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.TenantID != winner.TenantID {
		t.Errorf("stored identity points at %q, winner claimed %q", stored.TenantID, winner.TenantID)
	}

	// A returning user still updates in place: LinkOIDCIdentity keeps upserting.
	if err := s.LinkOIDCIdentity(ctx, &store.OIDCIdentity{
		Issuer: issuer, Subject: subject, UserID: winner.UserID,
		TenantID: winner.TenantID, Email: "renamed@example.com",
	}); err != nil {
		t.Fatalf("relink: %v", err)
	}
	stored, err = s.GetOIDCIdentity(ctx, issuer, subject)
	if err != nil {
		t.Fatalf("read back after relink: %v", err)
	}
	if stored.Email != "renamed@example.com" {
		t.Errorf("a returning user's identity did not update: email %q", stored.Email)
	}
}
