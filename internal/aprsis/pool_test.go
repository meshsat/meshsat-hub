package aprsis

import (
	"context"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

func newAccounts(t *testing.T) *integrations.Service {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 3)
	}
	svc := integrations.New(db, key)
	svc.DisableURLCheckForTest() // httptest binds to loopback; see the method doc
	return svc
}

// One tenant's unreachable server must not stop another tenant's connection
// being established, and must not take down the reconcile loop. This is the
// reason Reconcile logs per tenant and returns nothing: a customer who typed
// their server address wrongly is a support ticket, not an outage for everyone
// else on the Hub.
func TestOneTenantsBadServerDoesNotAffectAnother(t *testing.T) {
	accounts := newAccounts(t)
	ctx := context.Background()

	// 127.0.0.1:1 refuses immediately, so this is a fast, offline failure.
	if _, err := accounts.Set(ctx, "t_broken", integrations.ProviderAPRSIS, map[string]string{
		"callsign": "PD1BAD", "passcode": "12345", "server": "127.0.0.1:1", "ssid": "3",
	}); err != nil {
		t.Fatal(err)
	}

	platform, _ := wiredClient(t, "PI1MSH", 10)
	p := NewConnPool(platform, accounts, "default")

	p.Reconcile(ctx)

	// The broken tenant has no connection and therefore transmits nothing.
	if c := p.ForTenant("t_broken"); c != nil {
		t.Errorf("a tenant whose server refused the connection got a client %q", c.FormatCallsign())
	}
	// The platform's own connection was still established.
	if c := p.ForTenant("default"); c == nil {
		t.Fatal("one tenant's bad server address stopped the platform connecting: " +
			"a single customer's typo would silence everybody")
	}
}

// A tenant that pauses transmission keeps its callsign and passcode on file but
// stops going on air. Deleting the account is how you remove a licence; pausing
// for a weekend should not mean looking the passcode up again.
func TestPausedTransmissionStopsTheConnection(t *testing.T) {
	accounts := newAccounts(t)
	ctx := context.Background()

	platform, _ := wiredClient(t, "PI1MSH", 10)
	p := NewConnPool(platform, accounts, "default")
	p.Reconcile(ctx)
	if p.ForTenant("default") == nil {
		t.Fatal("the platform connection was not established")
	}

	// The default tenant's own row now says do not transmit; it must win over
	// the environment-built platform client.
	if _, err := accounts.Set(ctx, "default", integrations.ProviderAPRSIS, map[string]string{
		"callsign": "PI1MSH", "passcode": "12345", "server": "127.0.0.1:1", "enabled": "false",
	}); err != nil {
		t.Fatal(err)
	}
	p.Reconcile(ctx)

	if c := p.ForTenant("default"); c != nil {
		t.Errorf("a tenant that set enabled=false is still on air as %q", c.FormatCallsign())
	}
}

// Reconcile runs on a ticker while MQTT handlers call ForTenant on every
// position. They share one map, so the lock discipline is exercised directly
// here: `go test -race` cannot run in CI (CGO_ENABLED=0), which makes this the
// only place the check happens.
func TestReconcileAndForTenantAreSafeTogether(t *testing.T) {
	accounts := newAccounts(t)
	platform, _ := wiredClient(t, "PI1MSH", 10)
	p := NewConnPool(platform, accounts, "default")

	var wg sync.WaitGroup
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				p.Reconcile(ctx)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = p.ForTenant("default")
				_ = p.Tenants()
			}
		}()
	}
	wg.Wait()
	p.Close()
}
