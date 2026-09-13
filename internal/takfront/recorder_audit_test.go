package takfront

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
)

// MESHSAT-1110. Every pre-handshake failure has an empty tenant by
// construction, and the recorder returned early on one, so those refusals were
// counted and never written anywhere durable. The fix is deliberately narrow:
// one reason is audited, the rest stay out.

type auditSpy struct {
	mu   sync.Mutex
	rows []auditRow
	err  error
}

type auditRow struct{ tenant, action, actor, detail, ip string }

func (a *auditSpy) Log(_ context.Context, tenantID, action, actor, detail, ip string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, auditRow{tenantID, action, actor, detail, ip})
	return a.err
}

func (a *auditSpy) seen() []auditRow {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]auditRow(nil), a.rows...)
}

func addr(t *testing.T, s string) net.Addr {
	t.Helper()
	a, err := net.ResolveTCPAddr("tcp", s)
	if err != nil {
		t.Fatalf("addr: %v", err)
	}
	return a
}

// Reaching reasonUnidentified means the chain verified against a real tenant CA
// during the handshake and then failed on the re-read. Nothing else in the
// system records that it happened.
func TestAnUnattributableClientIsAuditedToThePlatformTenant(t *testing.T) {
	spy := &auditSpy{}
	r := NewRecorder(spy, "platform", nil)

	r.Refused(context.Background(), "", reasonUnidentified, addr(t, "203.0.113.9:44321"))

	rows := spy.seen()
	if len(rows) != 1 {
		t.Fatalf("wrote %d audit rows, want 1", len(rows))
	}
	if rows[0].tenant != "platform" {
		t.Errorf("audited to tenant %q, want the platform tenant", rows[0].tenant)
	}
	if rows[0].ip != "203.0.113.9" {
		t.Errorf("ip %q, want the client address -- it is the only identifying evidence there is", rows[0].ip)
	}
	if !strings.Contains(rows[0].detail, reasonUnidentified) {
		t.Errorf("detail %q does not name the reason", rows[0].detail)
	}
	if !strings.Contains(rows[0].detail, "directory may have changed") {
		t.Errorf("detail %q does not say what the event means; an operator reading this row "+
			"months later has only these words", rows[0].detail)
	}
}

// The guard that must not be weakened. Port 8089 faces the internet and is
// scanned constantly; auditing these would let anyone who can reach it drive an
// append-only SHA-256 hash chain.
func TestScannerNoiseIsCountedAndNeverAudited(t *testing.T) {
	for _, reason := range []string{reasonHandshake, reasonPlaintext, reasonProbe} {
		t.Run(reason, func(t *testing.T) {
			spy := &auditSpy{}
			r := NewRecorder(spy, "platform", nil)
			r.Refused(context.Background(), "", reason, addr(t, "198.51.100.7:1234"))
			if rows := spy.seen(); len(rows) != 0 {
				t.Errorf("%s wrote %d audit rows; an unauthenticated connection must not reach "+
					"the hash chain", reason, len(rows))
			}
		})
	}
}

// Unchanged behaviour: a refusal that names a tenant is still audited to that
// tenant, not to the platform.
func TestAnIdentifiedTenantsRefusalStillGoesToThatTenant(t *testing.T) {
	spy := &auditSpy{}
	r := NewRecorder(spy, "platform", nil)

	r.Refused(context.Background(), "t_real", reasonUnauthorized, addr(t, "192.0.2.5:9000"))

	rows := spy.seen()
	if len(rows) != 1 || rows[0].tenant != "t_real" {
		t.Fatalf("rows %+v, want one against t_real", rows)
	}
	if strings.Contains(rows[0].detail, "directory may have changed") {
		t.Error("an ordinary refusal picked up the unattributable explanation")
	}
}

// Without a platform tenant there is nowhere to put the entry, and inventing
// one would attribute a stranger's connection to a customer.
func TestNoPlatformTenantMeansNoUnattributedAudit(t *testing.T) {
	spy := &auditSpy{}
	r := NewRecorder(spy, "", nil)
	r.Refused(context.Background(), "", reasonUnidentified, addr(t, "203.0.113.9:1"))
	if rows := spy.seen(); len(rows) != 0 {
		t.Errorf("wrote %+v with no platform tenant configured", rows)
	}
}
