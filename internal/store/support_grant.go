package store

import "time"

// TenantSummary is a Tenant with the facts a platform operator's directory
// shows beside it (MESHSAT-1366). Counts are of rows the tenant owns now;
// they are not the quota's view, which internal/quota computes with the
// plan's ceiling.
type TenantSummary struct {
	Tenant
	// OwnerEmail is the address of Tenant.OwnerUserID, or of the first user
	// with the owner role when that column was never filled (tenants created
	// before it existed).
	OwnerEmail string `json:"owner_email,omitempty"`
	Users      int    `json:"users"`
	Devices    int    `json:"devices"`
	Bridges    int    `json:"bridges"`
}

// SupportGrant is a tenant owner's consent for the platform to open their
// workspace: a PIN only the customer knows, hashed at rest, and a window they
// chose (MESHSAT-1366). An operator's "open as this tenant" needs the PIN and
// an unexpired grant; the middleware then honours X-Tenant-ID for that tenant
// only while a grant has been OPENED (UsedAt set) and has not expired. The
// platform tenant never needs one for itself.
//
// Keyed (tenant_id, id), like every tenant-owned table.
type SupportGrant struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	// PINHash is Argon2id over the PIN (internal/auth.HashPassword). Never
	// serialised, listed in RedactedInExport.
	PINHash         string     `json:"-"`
	CreatedByUserID string     `json:"created_by_user_id,omitempty"`
	CreatedByEmail  string     `json:"created_by_email,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
	UsedAt          *time.Time `json:"used_at,omitempty"`
	UsedByEmail     string     `json:"used_by_email,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	FailedAttempts  int        `json:"failed_attempts"`
}

// Active reports whether the grant is usable at t: not revoked, not expired.
func (g *SupportGrant) Active(t time.Time) bool {
	return g != nil && g.RevokedAt == nil && t.Before(g.ExpiresAt)
}

// Opened reports whether an operator has already presented the PIN.
func (g *SupportGrant) Opened() bool { return g != nil && g.UsedAt != nil }
