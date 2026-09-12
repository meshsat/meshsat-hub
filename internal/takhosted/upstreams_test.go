package takhosted

import (
	"context"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// Upstreams decides where one tenant's CoT goes: their hosted instance, the server
// they run themselves (MESHSAT-1065), or both.

func hostedOnly(addr string) func(string) *takfront.Tenant {
	return func(id string) *takfront.Tenant {
		if id != "t1" {
			return nil
		}
		return &takfront.Tenant{TenantID: "t1", Label: "abcdefghij", Upstream: addr}
	}
}

// externalSource is a TAKConfigSource returning ready-made values.
type externalSource struct{ values map[string]map[string]string }

func (s externalSource) TAKUpstream(_ context.Context, tenantID string) (map[string]string, error) {
	return s.values[tenantID], nil
}

func withOwnServer(t *testing.T, host, port string) *ExternalUpstreams {
	t.Helper()
	v := goodTAKValues(t)
	v[takFieldHost] = host
	v[takFieldPort] = port
	return NewExternalUpstreams(externalSource{values: map[string]map[string]string{"t1": v}}, nil)
}

func TestAHostedInstanceAloneIsOneUpstream(t *testing.T) {
	u := NewUpstreams(nil, nil)
	u.SetHosted(hostedOnly("hosted.svc:8089"))

	got := u.For(context.Background(), "t1")
	if len(got) != 1 || got[0].Upstream != "hosted.svc:8089" {
		t.Fatalf("upstreams = %+v, want just the hosted one", got)
	}
	if n := len(u.For(context.Background(), "somebody-else")); n != 0 {
		t.Errorf("another tenant resolved %d upstreams, want 0", n)
	}
}

// The property that makes this independent of the TAK front: a tenant's own server
// resolves with NO hosted lookup set at all. The front needs a server certificate
// that does not exist yet, and a customer pointing the Hub at their own endpoint
// must not have to wait for it.
func TestATenantsOwnServerResolvesWithNoFrontAtAll(t *testing.T) {
	u := NewUpstreams(withOwnServer(t, "tak.acme.test", "8089"), nil)
	// Deliberately no SetHosted.

	got := u.For(context.Background(), "t1")
	if len(got) != 1 {
		t.Fatalf("upstreams = %+v, want the tenant's own server", got)
	}
	if got[0].Upstream != "tak.acme.test:8089" {
		t.Errorf("upstream = %q", got[0].Upstream)
	}
	if got[0].Label != "external" {
		t.Errorf("label = %q, want external so logs and metrics can tell them apart", got[0].Label)
	}
}

// Both, when a tenant has both -- the whole reason the forwarder fans out. Picking
// one would silently empty the other.
func TestATenantWithBothGetsBothHostedFirst(t *testing.T) {
	u := NewUpstreams(withOwnServer(t, "tak.acme.test", "8089"), nil)
	u.SetHosted(hostedOnly("hosted.svc:8089"))

	got := u.For(context.Background(), "t1")
	if len(got) != 2 {
		t.Fatalf("upstreams = %d, want 2 (hosted and their own)", len(got))
	}
	if got[0].Upstream != "hosted.svc:8089" {
		t.Errorf("first = %q, want the hosted instance: it is the map their phones are watching",
			got[0].Upstream)
	}
	if got[1].Upstream != "tak.acme.test:8089" {
		t.Errorf("second = %q, want their own server", got[1].Upstream)
	}
}

// A tenant who enters their own hosted instance as "their own server" would
// otherwise get two connections to it and every marker twice on one map. That is a
// plausible mistake and an unpleasant one to diagnose from the ATAK end.
func TestTheSameAddressTwiceIsOneUpstream(t *testing.T) {
	u := NewUpstreams(withOwnServer(t, "hosted.svc", "8089"), nil)
	u.SetHosted(hostedOnly("hosted.svc:8089"))

	got := u.For(context.Background(), "t1")
	if len(got) != 1 {
		t.Fatalf("upstreams = %d, want 1: the same address must not be dialled twice", len(got))
	}
}

// An unusable configuration contributes nothing and does not disturb the hosted
// instance: a customer mistyping their CA must not take their own fleet off their
// own map.
func TestAnUnusableOwnServerLeavesTheHostedOneAlone(t *testing.T) {
	bad := goodTAKValues(t)
	bad[takFieldCA] = "not a certificate"
	u := NewUpstreams(NewExternalUpstreams(
		externalSource{values: map[string]map[string]string{"t1": bad}}, nil), nil)
	u.SetHosted(hostedOnly("hosted.svc:8089"))

	got := u.For(context.Background(), "t1")
	if len(got) != 1 || got[0].Upstream != "hosted.svc:8089" {
		t.Fatalf("upstreams = %+v, want only the hosted one", got)
	}
}

func TestAnEmptyTenantOrNilResolverYieldsNothing(t *testing.T) {
	var nilU *Upstreams
	if got := nilU.For(context.Background(), "t1"); got != nil {
		t.Errorf("a nil resolver returned %+v", got)
	}
	u := NewUpstreams(nil, nil)
	if got := u.For(context.Background(), ""); got != nil {
		t.Errorf("an empty tenant id returned %+v", got)
	}
}
