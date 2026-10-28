package takhosted

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// MESHSAT-1711: after every rollout the front refused every phone of every tenant
// for up to five minutes, because a new pod asked for its upstream certificate on
// its first pass and only picked the signed one up on the next five-minute tick.
// These run the refresher's loop with an interval of an hour, so anything that
// happens within the test's few seconds can only be the retry.

// startLoop runs the refresher loop against the fake operator and returns the
// published directory as it changes.
func startLoop(t *testing.T, f *fakeAPI, p retryPolicy) *atomic.Pointer[takfront.Directory] {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	cl := NewClientWith(srv.Client(), srv.URL, "meshsat-tak")
	var got atomic.Pointer[takfront.Directory]
	r := NewDirectoryRefresher(cl, NewIdentityKeeper(cl, "replica-one", nil),
		func(d *takfront.Directory) { got.Store(d) }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.runEvery(ctx, time.Hour, p) }()
	t.Cleanup(func() { cancel(); <-done })
	return &got
}

func waitFor(t *testing.T, within time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func served(got *atomic.Pointer[takfront.Directory]) int {
	if d := got.Load(); d != nil {
		return d.Len()
	}
	return 0
}

func (f *fakeAPI) count(field *int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *field
}

// The case from production: the first pass asks, the operator signs a moment
// later, and the tenant must be served within seconds of the signature.
func TestATenantWaitingForTheHubsCertificateIsServedSoonAfterItIsSigned(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	f := newFakeAPI(ca)
	f.autoIssue = false
	f.setInstances(readyInstance("t-one", "aaaaaaaaaa", ca.pemStr))
	got := startLoop(t, f, retryPolicy{10 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond})

	if !waitFor(t, 2*time.Second, func() bool { return f.count(&f.creates) > 0 }) {
		t.Fatal("the first pass asked for no certificate")
	}
	if n := served(got); n != 0 {
		t.Fatalf("a tenant with no signed certificate was served: %d", n)
	}
	f.mu.Lock()
	for name, req := range f.reqs {
		req.Status = CertReqStat{Phase: CertIssued, CertPEM: ca.signCSR(t, req.Spec.CSRPEM, req.Spec.Username), Serial: "4242"}
		f.reqs[name] = req
	}
	f.mu.Unlock()

	if !waitFor(t, 2*time.Second, func() bool { return served(got) == 1 }) {
		t.Fatalf("the tenant was not served within 2 s of its certificate being signed (%d passes); "+
			"it waits for the next full interval", f.count(&f.lists))
	}
}

// Once nothing waits, the loop is back on its interval: a refresher that kept
// retrying would list the instances every few milliseconds for ever.
func TestTheRefresherStopsRetryingOnceEveryTenantIsServed(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	f := newFakeAPI(ca)
	f.autoIssue = true // signed on creation, collected on the next pass
	f.setInstances(readyInstance("t-one", "aaaaaaaaaa", ca.pemStr))
	got := startLoop(t, f, retryPolicy{5 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond})

	if !waitFor(t, 2*time.Second, func() bool { return served(got) == 1 }) {
		t.Fatal("the tenant was never served")
	}
	before := f.count(&f.lists)
	time.Sleep(300 * time.Millisecond)
	if after := f.count(&f.lists); after != before {
		t.Errorf("the refresher kept passing with every tenant served: %d passes became %d", before, after)
	}
}

// A first pass that fails (the API server busy as the pod starts) used to leave
// the empty startup directory published until the next tick.
func TestAFailedPassIsRetriedSoon(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	f := newFakeAPI(ca)
	f.autoIssue = true
	f.listErr = true
	f.setInstances(readyInstance("t-one", "aaaaaaaaaa", ca.pemStr))
	got := startLoop(t, f, retryPolicy{10 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond})

	if !waitFor(t, 2*time.Second, func() bool { return f.count(&f.lists) > 0 }) {
		t.Fatal("no pass ran")
	}
	f.mu.Lock()
	f.listErr = false
	f.mu.Unlock()
	if !waitFor(t, 2*time.Second, func() bool { return served(got) == 1 }) {
		t.Fatal("after a failed pass the tenant was not served within 2 s of the API server answering")
	}
}

// The retry doubles: an operator that never signs costs fewer and fewer passes,
// not one every base interval, and not one per cap from the start either.
func TestTheRetryDoublesWhileTheCertificateNeverComes(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	f := newFakeAPI(ca)
	f.autoIssue = false
	f.setInstances(readyInstance("t-one", "aaaaaaaaaa", ca.pemStr))
	startLoop(t, f, retryPolicy{5 * time.Millisecond, time.Second, time.Second})

	time.Sleep(500 * time.Millisecond)
	// Doubling from 5 ms: passes at 0, 5, 15, 35, 75, 155, 315 ms, about 7 in half
	// a second. A fixed 5 ms is about a hundred, a fixed one second is one.
	n := f.count(&f.lists)
	if n < 4 || n > 12 {
		t.Errorf("%d passes in 500 ms; doubling from 5 ms gives about 7", n)
	}
}

// However long the wait has been, a certificate signed late is picked up within
// the pending cap, not at the top of a long backoff.
func TestALateCertificateIsPickedUpWithinThePendingCap(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	f := newFakeAPI(ca)
	f.autoIssue = false
	f.setInstances(readyInstance("t-one", "aaaaaaaaaa", ca.pemStr))
	// Errors may back off to ten seconds; a pending tenant only to 40 ms.
	got := startLoop(t, f, retryPolicy{5 * time.Millisecond, 40 * time.Millisecond, 10 * time.Second})

	// Long enough that an uncapped doubling would be waiting over a second.
	time.Sleep(1300 * time.Millisecond)
	f.mu.Lock()
	for name, req := range f.reqs {
		req.Status = CertReqStat{Phase: CertIssued, CertPEM: ca.signCSR(t, req.Spec.CSRPEM, req.Spec.Username), Serial: "4242"}
		f.reqs[name] = req
	}
	f.mu.Unlock()
	signed := time.Now()
	if !waitFor(t, 2*time.Second, func() bool { return served(got) == 1 }) {
		t.Fatal("the late certificate was never picked up")
	}
	if d := time.Since(signed); d > 300*time.Millisecond {
		t.Errorf("picked up %v after signing; the pending cap is 40 ms", d.Round(time.Millisecond))
	}
}

// After a run of failed passes has grown the backoff, a tenant left only waiting
// for its certificate is back on the short cap at once, not after one more long
// wait.
func TestAfterErrorsAPendingTenantIsBackOnTheShortCap(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	f := newFakeAPI(ca)
	f.autoIssue = false
	f.listErr = true
	f.setInstances(readyInstance("t-one", "aaaaaaaaaa", ca.pemStr))
	got := startLoop(t, f, retryPolicy{5 * time.Millisecond, 40 * time.Millisecond, 10 * time.Second})

	// Errors for 1.3 s: the backoff is past a second by now.
	time.Sleep(1300 * time.Millisecond)
	f.mu.Lock()
	f.listErr = false
	f.mu.Unlock()
	// The next pass asks for the certificate; sign it as soon as it is asked for.
	if !waitFor(t, 3*time.Second, func() bool { return f.count(&f.creates) > 0 }) {
		t.Fatal("no certificate was asked for after the API server recovered")
	}
	f.mu.Lock()
	for name, req := range f.reqs {
		req.Status = CertReqStat{Phase: CertIssued, CertPEM: ca.signCSR(t, req.Spec.CSRPEM, req.Spec.Username), Serial: "4242"}
		f.reqs[name] = req
	}
	f.mu.Unlock()
	signed := time.Now()
	if !waitFor(t, 3*time.Second, func() bool { return served(got) == 1 }) {
		t.Fatal("the certificate was never picked up")
	}
	if d := time.Since(signed); d > 300*time.Millisecond {
		t.Errorf("picked up %v after signing; the pending cap is 40 ms", d.Round(time.Millisecond))
	}
}
