package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testFederation: peers and grants are per tenant, the default tenant is the
// victim, a fingerprint is unique per tenant (not across tenants), deleting
// a peer takes its grants with it (MESHSAT-1549).
func testFederation(t *testing.T, s store.Store) {
	ctx := context.Background()
	victim, other := store.DefaultTenantID, "fed-other"

	vp := &store.FederationPeer{Name: "peer hub", RelayURL: "https://peer.example/api/webhook/federation/tok", CertPEM: "pem-v", Fingerprint: "fp-1"}
	if err := s.CreateFederationPeer(ctx, victim, vp); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateFederationPeer(ctx, victim, &store.FederationPeer{RelayURL: "https://x", CertPEM: "p", Fingerprint: "fp-1"}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("second peer with the same fingerprint: %v", err)
	}
	// Another tenant may hold the same peer: fingerprints are unique per tenant.
	op := &store.FederationPeer{RelayURL: "https://peer.example/other", CertPEM: "pem-o", Fingerprint: "fp-1"}
	if err := s.CreateFederationPeer(ctx, other, op); err != nil {
		t.Fatalf("same fingerprint in another tenant: %v", err)
	}

	if got, err := s.GetFederationPeerByFingerprint(ctx, victim, "fp-1"); err != nil || got.ID != vp.ID || got.RelayURL != vp.RelayURL {
		t.Fatalf("victim lookup: %+v %v", got, err)
	}
	if _, err := s.GetFederationPeer(ctx, other, vp.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other tenant read the victim's peer: %v", err)
	}
	if list, _ := s.ListFederationPeers(ctx, other); len(list) != 1 || list[0].ID != op.ID {
		t.Fatalf("other tenant's list: %+v", list)
	}

	g := &store.FederationGrant{PeerID: vp.ID, FromIMEI: "300434060000001", ToIMEI: "300434060000002"}
	if err := s.CreateFederationGrant(ctx, victim, g); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateFederationGrant(ctx, victim, &store.FederationGrant{PeerID: vp.ID, FromIMEI: g.FromIMEI, ToIMEI: g.ToIMEI}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("identical grant: %v", err)
	}
	if ok, err := s.HasFederationGrant(ctx, victim, vp.ID, g.FromIMEI, g.ToIMEI); err != nil || !ok {
		t.Fatalf("victim grant not found: %v %v", ok, err)
	}
	if ok, _ := s.HasFederationGrant(ctx, other, vp.ID, g.FromIMEI, g.ToIMEI); ok {
		t.Fatalf("another tenant sees the victim's grant")
	}
	if ok, _ := s.HasFederationGrant(ctx, victim, vp.ID, g.ToIMEI, g.FromIMEI); ok {
		t.Fatalf("a grant works in the reverse direction")
	}
	if err := s.DeleteFederationGrant(ctx, other, g.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another tenant deleted the victim's grant: %v", err)
	}
	if err := s.DeleteFederationPeer(ctx, other, vp.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another tenant deleted the victim's peer: %v", err)
	}

	if err := s.DeleteFederationPeer(ctx, victim, vp.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasFederationGrant(ctx, victim, vp.ID, g.FromIMEI, g.ToIMEI); ok {
		t.Fatalf("grant survived its peer")
	}
	if list, _ := s.ListFederationGrants(ctx, victim); len(list) != 0 {
		t.Fatalf("grants left behind: %+v", list)
	}
	if list, _ := s.ListFederationPeers(ctx, other); len(list) != 1 {
		t.Fatalf("deleting the victim's peer touched another tenant: %+v", list)
	}
}
