package email

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// ReloadTopic tells every replica that a tenant's PGP contacts changed.
//
// The keyring is held in memory because Encrypt runs on every send, but the API
// writes contacts to the database and there are two Hub replicas. Without this a
// key added through the API is known to ONE of them, and whether an alert is
// encrypted depends on which replica happens to send it (MESHSAT-1123). Same
// shape as webhook.ReloadTopic and geo.ReloadTopic.
const ReloadTopic = "meshsat/hub/email-contacts/changed"

type reloadEvent struct {
	TenantID string `json:"tenant_id"`
}

// ErrNoGateway means this tenant has no outgoing mail configured. It is an error
// rather than a quiet no-op: an alert nobody can deliver should be visible.
var ErrNoGateway = errors.New("email: no outgoing mail configured for this tenant (Integrations page)")

// Gateway is one tenant's complete email setup: where its mail is sent from, and
// the PGP keys it signs with and encrypts to.
//
// The KeyRing is PER TENANT and that is the point. It used to be one global
// keyring whose contacts map was keyed by bare email address, so two tenants
// adding a contact for the same address collided -- the second overwrote the
// first, and one tenant's alert could then be encrypted to a key the OTHER
// tenant supplied and could read. Splitting the keyring fixes that by
// construction rather than by re-keying a shared map (MESHSAT-1121).
//
// Contacts are still in-memory and still per-replica; that is MESHSAT-1123 and is
// unchanged here.
type Gateway struct {
	Client  *Client
	KeyRing *KeyRing
}

// Pool resolves a tenant's email gateway.
type Pool struct {
	platform *Gateway
	accounts *integrations.Service
	cache    integrations.ClientCache[*Gateway]

	mu       sync.Mutex
	contacts ContactStore
	bus      bus.MessageBus
}

// ContactStore is the persistence the keyring needs. Declared here as a narrow
// interface rather than taking store.Store so this package keeps importing only
// what it uses.
type ContactStore interface {
	SaveEmailContact(ctx context.Context, tenantID, email, armoredKey string) error
	ListEmailContacts(ctx context.Context, tenantID string) ([]store.EmailContact, error)
	DeleteEmailContact(ctx context.Context, tenantID, email string) error
}

func NewPool(platform *Gateway, accounts *integrations.Service) *Pool {
	return &Pool{platform: platform, accounts: accounts}
}

// SetContactStore makes PGP contacts durable. Without it they behave as they did
// before MESHSAT-1123: in memory, lost on restart, and on one replica only.
func (p *Pool) SetContactStore(s ContactStore) {
	p.mu.Lock()
	p.contacts = s
	p.mu.Unlock()
}

func (p *Pool) contactStore() ContactStore {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.contacts
}

// SetBus subscribes to other replicas' contact changes. A nil bus means changes
// apply locally only.
func (p *Pool) SetBus(b bus.MessageBus) error {
	p.mu.Lock()
	p.bus = b
	p.mu.Unlock()
	if b == nil {
		return nil
	}
	return b.Subscribe(ReloadTopic, 1, func(_ string, payload []byte) {
		var ev reloadEvent
		if err := json.Unmarshal(payload, &ev); err != nil || ev.TenantID == "" {
			return
		}
		// Drop the cached gateway; the next ForTenant rebuilds it and reloads
		// the contacts. Cheaper and less error-prone than mutating a live
		// keyring underneath a send.
		p.cache.Forget(ev.TenantID)
	})
}

// AnnounceContactChange tells the other replicas to reload this tenant, and
// drops this replica's own cached gateway so the change applies here too.
//
// A failure to announce is logged, not returned: the contact IS saved, and the
// other replica picks it up when its gateway is next rebuilt. Failing the API
// call would be worse -- the customer would retry a write that already landed.
func (p *Pool) AnnounceContactChange(tenantID string) {
	p.cache.Forget(tenantID)
	p.mu.Lock()
	b := p.bus
	p.mu.Unlock()
	if b == nil {
		return
	}
	if err := b.PublishJSON(ReloadTopic, 1, false, reloadEvent{TenantID: tenantID}); err != nil {
		slog.Warn("email: announcing a contact change to the other replicas failed",
			"tenant", tenantID, "error", err)
	}
}

// Platform returns the operator's own gateway, or nil. Used by the parts of the
// Hub that are genuinely platform-level rather than per tenant.
func (p *Pool) Platform() *Gateway {
	if p == nil {
		return nil
	}
	return p.platform
}

// ForTenant returns the tenant's gateway, or nil when it has no outgoing mail.
func (p *Pool) ForTenant(ctx context.Context, tenantID string) *Gateway {
	if p == nil {
		return nil
	}
	if tenantID == "" {
		tenantID = store.DefaultTenantID
	}
	if p.accounts == nil {
		return p.platform
	}
	acct, err := p.accounts.ForTenant(ctx, tenantID, integrations.ProviderEmail)
	if err != nil {
		slog.Warn("email: resolving the tenant's account failed", "tenant", tenantID, "error", err)
		return nil
	}
	if acct == nil {
		// The default tenant keeps the environment-configured gateway; every
		// other tenant gets nil. An explicit tenant comparison, not a nil check
		// on p.platform, because the latter would hand the operator's sending
		// identity and PGP key to every customer.
		if tenantID == store.DefaultTenantID {
			return p.platform
		}
		return nil
	}
	if acct.Platform && p.platform != nil {
		return p.platform
	}

	host := acct.Get("smtp_host")
	if host == "" {
		// An inbound-only account: a webhook secret and nothing to send with.
		return nil
	}
	from, user, pass, key := acct.Get("from"), acct.Get("username"), acct.Get("password"), acct.Get("pgp_key")

	return p.cache.Get(tenantID, integrations.Fingerprint(host, from, user, pass, key), func() *Gateway {
		kr, err := NewKeyRing("MeshSat Hub", from, key)
		if err != nil {
			// A malformed key must not silently downgrade to no PGP at all: it
			// would send in cleartext for as long as nobody read the logs.
			slog.Error("email: the tenant's PGP key could not be loaded, no gateway",
				"tenant", tenantID, "error", err)
			return nil
		}
		p.loadContacts(ctx, tenantID, kr)
		return &Gateway{Client: NewClient(host, from, user, pass, kr), KeyRing: kr}
	})
}

// loadContacts fills a freshly built keyring from the database.
//
// A key that no longer parses is skipped and logged rather than failing the
// whole gateway: one bad row must not stop a tenant sending, and the fallback
// for that recipient is cleartext, which client.Send now warns about.
func (p *Pool) loadContacts(ctx context.Context, tenantID string, kr *KeyRing) {
	cs := p.contactStore()
	if cs == nil {
		return
	}
	rows, err := cs.ListEmailContacts(ctx, tenantID)
	if err != nil {
		slog.Error("email: loading this tenant's PGP contacts failed; mail to them "+
			"will be sent in CLEARTEXT", "tenant", tenantID, "error", err)
		return
	}
	for _, c := range rows {
		if err := kr.AddContact(c.Email, c.ArmoredKey); err != nil {
			slog.Error("email: a stored PGP contact key no longer parses; mail to "+
				"this address will be sent in CLEARTEXT",
				"tenant", tenantID, "email", c.Email, "error", err)
		}
	}
	slog.Debug("email: loaded PGP contacts", "tenant", tenantID, "count", len(rows))
}

// PersistContact stores a contact key and tells the other replica. Both steps
// matter: without the store the key dies at the next rollout, without the
// announce it is live on one replica of two.
func (p *Pool) PersistContact(ctx context.Context, tenantID, email, armoredKey string) error {
	cs := p.contactStore()
	if cs == nil {
		return nil // no store wired: pre-MESHSAT-1123 behaviour, in memory only
	}
	if err := cs.SaveEmailContact(ctx, tenantID, email, armoredKey); err != nil {
		return err
	}
	p.AnnounceContactChange(tenantID)
	return nil
}

// ForgetContact removes a stored contact key and tells the other replica.
func (p *Pool) ForgetContact(ctx context.Context, tenantID, email string) error {
	cs := p.contactStore()
	if cs == nil {
		return nil
	}
	if err := cs.DeleteEmailContact(ctx, tenantID, email); err != nil {
		return err
	}
	p.AnnounceContactChange(tenantID)
	return nil
}
