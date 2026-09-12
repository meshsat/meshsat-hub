package takfront

import (
	"context"
	"testing"
	"time"
)

// DialTenant exists so internal/takhosted's outbound forwarder does not
// reimplement this package's upstream TLS policy. Its comment claims it does not
// consume the tenant's PHONE budget, and a claim like that stops being true
// quietly, so it is asserted here.
//
// Why it matters: MaxConnsPerTenant (default 100) is what stops one tenant
// spending the front, and every upstream connection forks a process in
// OpenTAKServer. If the Hub's own forwarder took a slot from that budget, every
// tenant would silently lose a seat, and a tenant at its ceiling would have a
// real phone refused because the Hub was holding the last one.
func TestDialTenantDoesNotConsumeTheTenantPhoneBudget(t *testing.T) {
	ots := newFakeOTS(t, "budget")
	phoneCA := newTestCA(t, "tenant-budget-ca")
	tenant := tenantOn(t, "budget", phoneCA, ots)
	tenant.MaxConns = 1 // a ceiling of exactly one phone
	f := startFront(t, Config{}, allowAll{}, []Tenant{tenant})

	if _, before := f.srv.Conns(tenant.TenantID); before != 0 {
		t.Fatalf("tenant connections = %d before anything connected", before)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := f.srv.DialTenant(ctx, &tenant)
	if err != nil {
		t.Fatalf("DialTenant: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// The connection is real: it reached the fake OpenTAKServer, which requires
	// and verifies a client certificate. So this is not passing because nothing
	// happened.
	if conn.RemoteAddr().String() != ots.addr {
		t.Errorf("dialled %s, want the tenant upstream %s", conn.RemoteAddr(), ots.addr)
	}

	if _, after := f.srv.Conns(tenant.TenantID); after != 0 {
		t.Errorf("the tenant's phone budget was charged %d for the Hub's own connection; "+
			"with MaxConns=%d that costs the tenant its only seat", after, tenant.MaxConns)
	}
}

// And a nil tenant is refused rather than panicking on a dereference, since this
// is now an exported entry point reachable from another package.
func TestDialTenantRefusesANilTenant(t *testing.T) {
	ots := newFakeOTS(t, "nil")
	phoneCA := newTestCA(t, "tenant-nil-ca")
	f := startFront(t, Config{}, allowAll{}, []Tenant{tenantOn(t, "nil", phoneCA, ots)})

	if _, err := f.srv.DialTenant(context.Background(), nil); err == nil {
		t.Error("DialTenant(nil) returned no error")
	}
}

// The policy DialTenant exists to share: when a tenant sets no
// UpstreamServerName, the chain is still verified against that tenant's CA and
// only the NAME check is skipped. A reimplementation that set
// InsecureSkipVerify without the VerifyConnection callback would accept any
// certificate from anything answering on that address, and would look correct.
//
// This proves the verification is real by pointing a tenant at an upstream whose
// certificate was issued by a DIFFERENT authority.
func TestDialTenantStillVerifiesTheChainWhenTheNameCheckIsSkipped(t *testing.T) {
	good := newFakeOTS(t, "good")
	impostor := newFakeOTS(t, "impostor")
	phoneCA := newTestCA(t, "tenant-verify-ca")

	tenant := tenantOn(t, "verify", phoneCA, good)
	// Point the tenant at the impostor while keeping the good CA as its trust
	// anchor: the address answers, presents a valid certificate, and is signed by
	// somebody else.
	tenant.Upstream = impostor.addr
	tenant.UpstreamServerName = "" // the skip-the-name-check path

	f := startFront(t, Config{}, allowAll{}, []Tenant{tenant})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := f.srv.DialTenant(ctx, &tenant)
	if err == nil {
		_ = conn.Close()
		t.Fatal("an upstream signed by a different CA was accepted; skipping the name " +
			"check must not mean skipping verification")
	}
}
