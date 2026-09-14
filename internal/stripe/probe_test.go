package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/health"
)

func endpointServer(t *testing.T, status int, body string, calls *atomic.Int32) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		if r.URL.Path != "/webhook_endpoints" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := NewClient("sk_test_probe", time.Second)
	c.SetBaseURL(srv.URL)
	return c
}

func TestStripeProbeReportsWhatStripeSaid(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error // nil means healthy; otherwise errors.Is target, or any error when set to errAny
	}{
		{"enabled endpoint", 200, `{"data":[{"id":"we_1","status":"enabled","api_version":"2025-08-27.basil"}]}`, nil},
		{"only a disabled endpoint", 200, `{"data":[{"id":"we_1","status":"disabled"}]}`, ErrNoEnabledEndpoint},
		{"no endpoint at all", 200, `{"data":[]}`, ErrNoEnabledEndpoint},
		{"stripe refuses", 500, `{"error":{"message":"boom"}}`, errAny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProbe(endpointServer(t, tc.status, tc.body, nil), time.Hour)
			if err := p.Health(context.Background()); !errors.Is(err, ErrNotCheckedYet) {
				t.Fatalf("before the first check Health must say so, got %v", err)
			}
			err := p.Check(context.Background())
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("expected healthy, got %v", err)
			case tc.want == errAny && err == nil:
				t.Fatal("expected an error from a 500, got nil")
			case tc.want != nil && tc.want != errAny && !errors.Is(err, tc.want):
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			if got := p.Health(context.Background()); !sameOutcome(got, err) {
				t.Fatalf("Health must report the recorded result: check=%v health=%v", err, got)
			}
			if p.LastChecked().IsZero() {
				t.Fatal("LastChecked not recorded")
			}
		})
	}
}

var errAny = errors.New("any error")

func sameOutcome(a, b error) bool { return (a == nil) == (b == nil) }

// TestStripeProbeHealthNeverCallsStripe is the reason the probe exists: /readyz
// runs every ten seconds on every replica, and a network call per evaluation
// would put Stripe on the readiness path.
func TestStripeProbeHealthNeverCallsStripe(t *testing.T) {
	var calls atomic.Int32
	p := NewProbe(endpointServer(t, 200, `{"data":[{"id":"we_1","status":"enabled"}]}`, &calls), time.Hour)
	_ = p.Check(context.Background())
	before := calls.Load()
	for i := 0; i < 50; i++ {
		_ = p.Health(context.Background())
	}
	if calls.Load() != before {
		t.Fatalf("Health made %d Stripe calls; it must make none", calls.Load()-before)
	}
}

// TestStripeProbeNeverAffectsReadiness: a Stripe outage must show on the
// status page and must not take the Hub out of the Service. This is the
// AddInfoProbe contract, asserted end to end through /readyz because the
// registration in main.go is exactly one identifier away from AddProbe.
func TestStripeProbeNeverAffectsReadiness(t *testing.T) {
	p := NewProbe(endpointServer(t, 503, `down`, nil), time.Hour)
	if err := p.Check(context.Background()); err == nil {
		t.Fatal("fixture: the check should fail")
	}
	checker := health.New(time.Second)
	checker.AddInfoProbe("stripe", p.Health)

	rec := httptest.NewRecorder()
	checker.ReadyzHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("a failing stripe probe changed /readyz to %d; it must stay 200", rec.Code)
	}
	var resp health.Response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" {
		t.Fatalf("readiness status %q, want ok", resp.Status)
	}
	if _, listed := resp.Checks["stripe"]; listed {
		t.Fatal("stripe appeared among the CRITICAL checks; it must be informational")
	}
}
