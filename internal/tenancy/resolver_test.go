package tenancy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

type stubStore struct {
	devices map[string]string
	bridges map[string]string
	calls   int
	err     error
}

func (s *stubStore) LookupDeviceTenant(_ context.Context, imei string) (string, error) {
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	if t, ok := s.devices[imei]; ok {
		return t, nil
	}
	return "", store.ErrNotFound
}

func (s *stubStore) LookupBridgeTenant(_ context.Context, id string) (string, error) {
	s.calls++
	if t, ok := s.bridges[id]; ok {
		return t, nil
	}
	return "", store.ErrNotFound
}

func TestResolver_KnownUnknownAndCache(t *testing.T) {
	st := &stubStore{devices: map[string]string{"111": "tenant-a"}, bridges: map[string]string{"b1": "tenant-b"}}
	r := NewResolver(st, "default", time.Minute)
	ctx := context.Background()
	if got := r.ForDevice(ctx, "111"); got != "tenant-a" {
		t.Fatalf("known device: %q", got)
	}
	if got := r.ForDevice(ctx, "999"); got != "default" {
		t.Fatalf("unknown device: %q", got)
	}
	if got := r.ForBridge(ctx, "b1"); got != "tenant-b" {
		t.Fatalf("known bridge: %q", got)
	}
	calls := st.calls
	r.ForDevice(ctx, "111")
	r.ForDevice(ctx, "999")
	r.ForBridge(ctx, "b1")
	if st.calls != calls {
		t.Fatalf("cache miss: %d calls after %d", st.calls, calls)
	}
	r.Forget("111")
	r.ForDevice(ctx, "111")
	if st.calls != calls+1 {
		t.Fatalf("Forget must invalidate: %d", st.calls)
	}
}

func TestResolver_CacheExpiry(t *testing.T) {
	st := &stubStore{devices: map[string]string{"111": "tenant-a"}}
	r := NewResolver(st, "default", time.Minute)
	now := time.Now()
	r.now = func() time.Time { return now }
	r.ForDevice(context.Background(), "111")
	now = now.Add(2 * time.Minute)
	r.ForDevice(context.Background(), "111")
	if st.calls != 2 {
		t.Fatalf("expired entry must be refreshed: %d calls", st.calls)
	}
}

func TestResolver_StoreErrorFallsBackWithoutCaching(t *testing.T) {
	st := &stubStore{err: errors.New("db down")}
	r := NewResolver(st, "default", time.Minute)
	for i := 0; i < 3; i++ {
		if got := r.ForDevice(context.Background(), "111"); got != "default" {
			t.Fatalf("fallback: %q", got)
		}
	}
	if st.calls != 3 {
		t.Fatalf("errors must not be cached: %d calls", st.calls)
	}
}

func TestResolver_AmbiguousUsesDefault(t *testing.T) {
	st := &stubStore{err: store.ErrAmbiguousTenant}
	r := NewResolver(st, "default", 0)
	if got := r.ForDevice(context.Background(), "111"); got != "default" {
		t.Fatalf("ambiguous: %q", got)
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := WithTenant(context.Background(), "t1")
	if FromContext(ctx) != "t1" || FromContext(context.Background()) != "" {
		t.Fatal("context helpers")
	}
}
