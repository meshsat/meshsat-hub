package takhosted

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// The front refuses to exist without an Authorizer, so this assertion is what
// makes a signature drift in takfront a compile error here rather than a runtime
// type mismatch at startup.
var _ takfront.Authorizer = (*Authorizer)(nil)

// Authorizer answers the one question takfront asks on every connection: this
// certificate verified against the tenant's CA, but is the person still allowed?
//
// A certificate is a bearer token with a 90-day life. Everything that can change
// in those 90 days -- a teammate leaving, a lost phone, a tenant suspended for
// non-payment, a seat removed -- has to be answered from current state, not from
// the certificate. That is why there is no CRL: the front asks here, in process,
// on every connection, so a revocation takes effect on the next reconnect with
// nothing to distribute and nothing to expire.
//
// It is deliberately NOT a quota check. A tenant over its TAK ceiling keeps every
// account it has; the ceiling gates creating one. A phone already enrolled keeps
// connecting, keeps sending position reports and keeps its SOS path whatever the
// plan says.
type Authorizer struct {
	store store.Store

	// tenantActive reports whether a tenant may be served at all. Supplied from
	// tenancy.StatusCache, which already invalidates across replicas when a
	// status changes, so a suspension applies everywhere at once rather than at
	// the end of some local TTL.
	tenantActive func(ctx context.Context, tenantID string) (bool, error)

	// cache holds recent decisions. A phone reconnects on every network change,
	// and ATAK reconnects briskly; without this, a flapping mobile link would
	// drive a database read per attempt.
	mu    sync.Mutex
	cache map[string]cachedDecision
	ttl   time.Duration
	now   func() time.Time
}

type cachedDecision struct {
	err error // nil means allowed
	at  time.Time
}

// Refusals a caller may want to distinguish. They are returned to takfront,
// which turns them into a refusal reason and an audit entry.
var (
	// ErrNoSuchUser means the certificate's common name matches no TAK account
	// in that tenant. This is also what a deleted user looks like.
	ErrNoSuchUser = errors.New("takhosted: no such TAK user")
	// ErrUserInactive means the account exists and is switched off.
	ErrUserInactive = errors.New("takhosted: TAK user is deactivated")
	// ErrCertRevoked means this exact certificate serial was revoked, even
	// though the account may still be active with a newer certificate.
	ErrCertRevoked = errors.New("takhosted: certificate was revoked")
	// ErrTenantInactive means the tenant is suspended or deleted.
	ErrTenantInactive = errors.New("takhosted: tenant is not active")
)

// DefaultAuthzTTL is how long a decision is reused. Short, because it bounds how
// long a revoked phone keeps reconnecting: at 30 seconds a removed teammate is
// out within half a minute, and a reconnect storm still costs one read per
// tenant-user per half minute rather than one per attempt.
const DefaultAuthzTTL = 30 * time.Second

// NewAuthorizer builds the authorizer takfront consults.
//
// tenantActive may be nil, in which case tenant status is not checked here --
// acceptable only in tests, because a suspended tenant's phones would otherwise
// keep streaming.
func NewAuthorizer(s store.Store, tenantActive func(ctx context.Context, tenantID string) (bool, error)) *Authorizer {
	return &Authorizer{
		store:        s,
		tenantActive: tenantActive,
		cache:        map[string]cachedDecision{},
		ttl:          DefaultAuthzTTL,
		now:          time.Now,
	}
}

// WithTTL overrides the decision cache lifetime. Zero disables caching, which is
// what a test asserting revocation timing wants.
func (a *Authorizer) WithTTL(d time.Duration) *Authorizer {
	a.ttl = d
	return a
}

// AllowTAKClient implements takfront.Authorizer.
//
// The signature is takfront's: the tenant comes from the certificate's issuer,
// the common name from its subject, and the serial from the certificate itself.
// All three are already verified against the tenant's CA by the time this runs,
// so this function never re-does cryptography -- it only asks whether the named
// account is still entitled to connect.
//
// A database failure REFUSES the connection. That is the opposite of the quota
// check, which fails open, and the difference is deliberate: failing open there
// lets somebody register one device too many, while failing open here would admit
// a revoked phone into a tenant's live map. The conservative direction is not the
// same in both places.
func (a *Authorizer) AllowTAKClient(ctx context.Context, tenantID, commonName string, serial *big.Int) error {
	if a == nil || a.store == nil {
		return errors.New("takhosted: authorizer has no store")
	}
	if tenantID == "" || commonName == "" {
		return ErrNoSuchUser
	}
	serialStr := ""
	if serial != nil {
		serialStr = serial.String()
	}

	key := tenantID + "\x00" + commonName + "\x00" + serialStr
	if a.ttl > 0 {
		a.mu.Lock()
		if d, ok := a.cache[key]; ok && a.now().Sub(d.at) < a.ttl {
			a.mu.Unlock()
			return d.err
		}
		a.mu.Unlock()
	}

	err := a.decide(ctx, tenantID, commonName, serialStr)

	// Only a decision is cached, never a transient failure: a database blip must
	// not pin a refusal for the whole TTL.
	if a.ttl > 0 && isDecision(err) {
		a.mu.Lock()
		a.cache[key] = cachedDecision{err: err, at: a.now()}
		a.mu.Unlock()
	}
	return err
}

func (a *Authorizer) decide(ctx context.Context, tenantID, commonName, serial string) error {
	if a.tenantActive != nil {
		ok, err := a.tenantActive(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("takhosted: tenant status unavailable: %w", err)
		}
		if !ok {
			return ErrTenantInactive
		}
	}

	u, err := a.store.GetTAKUser(ctx, tenantID, commonName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNoSuchUser
		}
		return fmt.Errorf("takhosted: reading the TAK user: %w", err)
	}
	if !u.Active {
		return ErrUserInactive
	}
	// A revoked serial is refused even when the account is active: the person is
	// still a member, this particular certificate is not theirs any more.
	if serial != "" && u.RevokedSerial != "" && strings.EqualFold(u.RevokedSerial, serial) {
		return ErrCertRevoked
	}
	return nil
}

// isDecision reports whether err is a settled answer about entitlement rather
// than a failure to find out.
func isDecision(err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrNoSuchUser), errors.Is(err, ErrUserInactive),
		errors.Is(err, ErrCertRevoked), errors.Is(err, ErrTenantInactive):
		return true
	default:
		return false
	}
}

// Forget drops any cached decision for one user, so a revocation or a
// deactivation applies on the next connection instead of at the end of the TTL.
// The API calls this when it changes a TAK user.
func (a *Authorizer) Forget(tenantID, commonName string) {
	if a == nil {
		return
	}
	prefix := tenantID + "\x00" + commonName + "\x00"
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.cache {
		if strings.HasPrefix(k, prefix) {
			delete(a.cache, k)
		}
	}
}

// ForgetTenant drops every cached decision for a tenant, for a suspension or a
// purge.
func (a *Authorizer) ForgetTenant(tenantID string) {
	if a == nil {
		return
	}
	prefix := tenantID + "\x00"
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.cache {
		if strings.HasPrefix(k, prefix) {
			delete(a.cache, k)
		}
	}
}
