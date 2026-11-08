package tenancy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

const defaultTenant = store.DefaultTenantID

func TestAdmitDeviceTopic(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name, id, topic string
		wantTenant      string
		wantOK          bool
	}{
		{"owner in its own namespace", "owned-by-a", "tenant-a", "tenant-a", true},
		{"platform device on its own topic", "owned-by-default", defaultTenant, defaultTenant, true},
		// The default tenant as the victim: the case MESHSAT-1469 is about.
		{"customer naming the platform's device", "owned-by-default", "t_known", "", false},
		{"customer naming another customer's device", "owned-by-a", "t_known", "", false},
		// Only the Hub and the operator's own bridges can write these.
		{"platform namespace names a customer's device", "owned-by-a", defaultTenant, "tenant-a", true},
		{"no namespace at all", "owned-by-a", "", "tenant-a", true},
		{"unregistered, real tenant", "nobody", "t_known", "t_known", true},
		{"unregistered, unknown tenant", "nobody", "t_ghost", defaultTenant, true},
		{"unregistered, no namespace", "nobody", "", defaultTenant, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &stubStore{devices: map[string]string{"owned-by-a": "tenant-a", "owned-by-default": defaultTenant}}
			r := NewResolver(s, defaultTenant, 0)
			got, ok := r.AdmitDeviceTopic(ctx, c.id, c.topic)
			if got != c.wantTenant || ok != c.wantOK {
				t.Errorf("AdmitDeviceTopic(%q, %q) = %q, %v; want %q, %v", c.id, c.topic, got, ok, c.wantTenant, c.wantOK)
			}
		})
	}
}

func TestAdmitBridgeTopicRefusesAnotherTenantsBridge(t *testing.T) {
	r := NewResolver(&stubStore{bridges: map[string]string{"platform-kit": defaultTenant}}, defaultTenant, 0)
	if got, ok := r.AdmitBridgeTopic(context.Background(), "platform-kit", "t_known"); ok || got != "" {
		t.Errorf("a customer's topic naming the platform's bridge: %q, %v", got, ok)
	}
	if got, ok := r.AdmitBridgeTopic(context.Background(), "platform-kit", defaultTenant); !ok || got != defaultTenant {
		t.Errorf("the platform's own bridge: %q, %v", got, ok)
	}
}

// A phone number has no owner of record, whatever row an older Hub let somebody
// create, and finding that out costs the store nothing.
func TestAPhoneNumberHasNoOwnerOfRecord(t *testing.T) {
	s := &stubStore{devices: map[string]string{"+31612345678": "tenant-a"}}
	r := NewResolver(s, defaultTenant, 0)
	if got, ok := r.AdmitDeviceTopic(context.Background(), "+31612345678", defaultTenant); !ok || got != defaultTenant {
		t.Errorf("on the platform's namespace: %q, %v", got, ok)
	}
	if got, ok := r.AdmitDeviceTopic(context.Background(), "+31612345678", "t_known"); !ok || got != "t_known" {
		t.Errorf("on a customer's namespace: %q, %v", got, ok)
	}
	if s.calls != 0 {
		t.Errorf("%d owner lookups for a phone number", s.calls)
	}
}

func clocked(s *stubStore, ttl time.Duration) (*Resolver, *time.Time) {
	r := NewResolver(s, defaultTenant, ttl)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	return r, &now
}

// A modem that changed hands inside the cache's TTL is not a forgery. The real
// message of the new owner must get through on a replica that is behind.
func TestAdmitReadsTheStoreAgainBeforeRefusing(t *testing.T) {
	ctx := context.Background()
	s := &stubStore{devices: map[string]string{"modem": defaultTenant}}
	r, now := clocked(s, 30*time.Second)
	if _, ok := r.AdmitDeviceTopic(ctx, "modem", defaultTenant); !ok {
		t.Fatal("seeding the cache")
	}

	s.devices["modem"] = "t_known"
	*now = now.Add(5 * time.Second)

	if got, ok := r.AdmitDeviceTopic(ctx, "modem", "t_known"); !ok || got != "t_known" {
		t.Errorf("the new owner's message: %q, %v", got, ok)
	}
}

// Forgeries as fast as the broker allows must not become a database load.
func TestARefusalReadsTheStoreAtMostOnceASecond(t *testing.T) {
	ctx := context.Background()
	s := &stubStore{devices: map[string]string{"modem": defaultTenant}}
	r, now := clocked(s, 30*time.Second)
	r.AdmitDeviceTopic(ctx, "modem", "t_known")
	first := s.calls
	if first != 1 {
		t.Fatalf("the first look took %d reads", first)
	}

	for range 100 {
		if _, ok := r.AdmitDeviceTopic(ctx, "modem", "t_known"); ok {
			t.Fatal("a forgery was admitted")
		}
	}
	if s.calls != first {
		t.Errorf("100 refusals inside a second made %d more store reads", s.calls-first)
	}

	*now = now.Add(ownerRecheckEvery)
	for range 100 {
		r.AdmitDeviceTopic(ctx, "modem", "t_known")
	}
	if s.calls != first+1 {
		t.Errorf("after the interval: %d store reads, want 1 more", s.calls-first)
	}
}

// With the store down the cached owner still answers: a forgery does not get
// through because the database hiccuped.
func TestARefusalStandsWhenTheStoreCannotAnswer(t *testing.T) {
	ctx := context.Background()
	s := &stubStore{devices: map[string]string{"modem": defaultTenant}}
	r, now := clocked(s, 30*time.Second)
	r.AdmitDeviceTopic(ctx, "modem", defaultTenant)

	s.err = errors.New("connection refused")
	*now = now.Add(5 * time.Second)

	if got, ok := r.AdmitDeviceTopic(ctx, "modem", "t_known"); ok || got != "" {
		t.Errorf("with the store down: %q, %v", got, ok)
	}
}

// With nothing cached and the store down the device is simply unknown, and a
// real message must not be lost to a failed lookup.
func TestAFailedLookupAdmits(t *testing.T) {
	r := NewResolver(&stubStore{err: errors.New("connection refused")}, defaultTenant, 30*time.Second)
	if got, ok := r.AdmitDeviceTopic(context.Background(), "modem", defaultTenant); !ok || got != defaultTenant {
		t.Errorf("failed lookup: %q, %v", got, ok)
	}
}

func TestRefreshDeviceCorrectsTheCache(t *testing.T) {
	ctx := context.Background()
	s := &stubStore{devices: map[string]string{"modem": "tenant-a"}}
	r, _ := clocked(s, 30*time.Second)
	if got := r.ForDevice(ctx, "modem"); got != "tenant-a" {
		t.Fatalf("seeding: %q", got)
	}

	s.devices["modem"] = "tenant-b"
	if got := r.ForDevice(ctx, "modem"); got != "tenant-a" {
		t.Fatalf("the cache should still answer: %q", got)
	}

	r.RefreshDevice(ctx, "modem")
	if got := r.ForDevice(ctx, "modem"); got != "tenant-b" {
		t.Errorf("after a refresh: %q", got)
	}
}

func TestAFailedRefreshKeepsWhatTheCacheHad(t *testing.T) {
	ctx := context.Background()
	s := &stubStore{devices: map[string]string{"modem": "tenant-a"}}
	r, _ := clocked(s, 30*time.Second)
	r.ForDevice(ctx, "modem")

	s.err = errors.New("connection refused")
	r.RefreshDevice(ctx, "modem")

	if got := r.ForDevice(ctx, "modem"); got != "tenant-a" {
		t.Errorf("a failed refresh changed the answer to %q", got)
	}
}
