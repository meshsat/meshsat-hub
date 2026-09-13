package cloudloop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// MESHSAT-1118. The send budget is per (tenant, device), and the tenant the
// send path hands the limiter has to be the one that OWNS the device -- not the
// caller's, and not a blank.
//
// This is the wiring the other tests in internal/ratelimit cannot see. If this
// call passed the wrong tenant, every isolation test there would still pass
// while production keyed every device under one namespace again.

// recordingLimiter captures what the send path asked.
type recordingLimiter struct {
	mu   sync.Mutex
	seen []string // "tenant|device|isSOS"
}

func (r *recordingLimiter) Allow(tenantID, deviceID string, isSOS bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	sos := "n"
	if isSOS {
		sos = "y"
	}
	r.seen = append(r.seen, tenantID+"|"+deviceID+"|"+sos)
	return true
}

func (r *recordingLimiter) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// ownerStore answers the tenancy resolver: this device belongs to this tenant.
type ownerStore struct{ owner map[string]string }

func (s ownerStore) LookupDeviceTenant(_ context.Context, imei string) (string, error) {
	if t, ok := s.owner[imei]; ok {
		return t, nil
	}
	return "", store.ErrNotFound
}
func (s ownerStore) LookupBridgeTenant(context.Context, string) (string, error) {
	return "", store.ErrNotFound
}
func (s ownerStore) GetTenant(_ context.Context, id string) (*store.Tenant, error) {
	return &store.Tenant{ID: id}, nil
}

func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MTResponse{ID: "sbd-1", Status: "queued"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTheSendPathChargesTheDevicesOwnTenant(t *testing.T) {
	srv := okServer(t)
	const imei = "300434065000001"
	const owner = "t_alpha"

	lim := &recordingLimiter{}
	s := NewSender(NewClient(srv.URL, "k"), &recordingBus{})
	s.SetDeviceResolver(staticResolver{thing: "THING-abc"})
	s.SetRateLimiter(lim)
	s.SetTenants(tenancy.NewResolver(
		ownerStore{owner: map[string]string{imei: owner}}, store.DefaultTenantID, time.Minute))

	if _, err := s.SendDirect(imei, MTSendRequest{Text: "hi"}); err != nil {
		t.Fatalf("SendDirect: %v", err)
	}

	got := lim.calls()
	if len(got) != 1 {
		t.Fatalf("the limiter was consulted %d times, want 1: %v", len(got), got)
	}
	if got[0] != owner+"|"+imei+"|n" {
		t.Errorf("the send path charged %q; want the device's owning tenant %q.\n"+
			"A blank or wrong tenant here re-collapses every device onto one shared "+
			"budget, and the isolation tests in internal/ratelimit cannot see it.",
			got[0], owner)
	}
}

// A device nobody has registered still gets a named tenant -- the default --
// never an empty one.
func TestAnUnregisteredDeviceStillNamesATenant(t *testing.T) {
	srv := okServer(t)
	lim := &recordingLimiter{}
	s := NewSender(NewClient(srv.URL, "k"), &recordingBus{})
	s.SetDeviceResolver(staticResolver{thing: "THING-abc"})
	s.SetRateLimiter(lim)
	s.SetTenants(tenancy.NewResolver(ownerStore{owner: map[string]string{}}, store.DefaultTenantID, time.Minute))

	if _, err := s.SendDirect("300434065999999", MTSendRequest{Text: "hi"}); err != nil {
		t.Fatalf("SendDirect: %v", err)
	}
	got := lim.calls()
	if len(got) != 1 || got[0] != store.DefaultTenantID+"|300434065999999|n" {
		t.Errorf("limiter calls = %v, want the default tenant named explicitly", got)
	}
}

// And with no resolver configured at all -- a single-tenant deployment -- the
// tenant is still named rather than left blank.
func TestASenderWithNoResolverStillNamesTheDefaultTenant(t *testing.T) {
	srv := okServer(t)
	lim := &recordingLimiter{}
	s := NewSender(NewClient(srv.URL, "k"), &recordingBus{})
	s.SetDeviceResolver(staticResolver{thing: "THING-abc"})
	s.SetRateLimiter(lim)

	if _, err := s.SendDirect("300434065000002", MTSendRequest{Text: "hi"}); err != nil {
		t.Fatalf("SendDirect: %v", err)
	}
	got := lim.calls()
	if len(got) != 1 || got[0] != store.DefaultTenantID+"|300434065000002|n" {
		t.Errorf("limiter calls = %v, want the default tenant named explicitly", got)
	}
}

// The MQTT send path is the one that carries real traffic, and it had no test
// of its own -- a mutation blanking its tenant survived the first sweep while
// every other call site was covered.
func TestTheMQTTSendPathChargesTheDevicesOwnTenant(t *testing.T) {
	srv := okServer(t)
	const imei = "300434065000003"
	const owner = "t_beta"

	lim := &recordingLimiter{}
	s := NewSender(NewClient(srv.URL, "k"), &recordingBus{})
	s.SetDeviceResolver(staticResolver{thing: "THING-abc"})
	s.SetRateLimiter(lim)
	s.SetTenants(tenancy.NewResolver(
		ownerStore{owner: map[string]string{imei: owner}}, store.DefaultTenantID, time.Minute))

	body, _ := json.Marshal(MTSendRequest{Text: "hi"})
	s.handleMTSend("meshsat/"+owner+"/"+imei+"/mt/send", body)

	got := lim.calls()
	if len(got) != 1 {
		t.Fatalf("the limiter was consulted %d times, want 1: %v", len(got), got)
	}
	if got[0] != owner+"|"+imei+"|n" {
		t.Errorf("the MQTT send path charged %q, want %q|%s|n", got[0], owner, imei)
	}
}

// An SOS must still reach the limiter marked as one, whatever the tenant
// resolution did -- the bypass is what carries a distress message out when the
// budget is spent.
func TestAnSOSOnTheMQTTPathIsMarkedAsOne(t *testing.T) {
	srv := okServer(t)
	const imei = "300434065000004"

	lim := &recordingLimiter{}
	s := NewSender(NewClient(srv.URL, "k"), &recordingBus{})
	s.SetDeviceResolver(staticResolver{thing: "THING-abc"})
	s.SetRateLimiter(lim)

	body, _ := json.Marshal(MTSendRequest{Text: "SOS", Priority: 9})
	s.handleMTSend("meshsat/"+imei+"/mt/send", body)

	got := lim.calls()
	if len(got) != 1 || got[0] != store.DefaultTenantID+"|"+imei+"|y" {
		t.Errorf("limiter calls = %v, want the SOS flag set", got)
	}
}
