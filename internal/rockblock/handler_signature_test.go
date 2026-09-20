package rockblock

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
	"github.com/meshsat/meshsat-hub/internal/webhookroute"
)

// A signature is verified wherever it appears (MESHSAT-1247).
//
// The per-tenant capability path used to skip the check entirely: whoever held
// the URL was believed, and a JWT presented alongside it was never looked at,
// so a forged token rode in exactly as a real one did. These cases pin the
// behaviour on THAT path, because the platform path was already covered.
func TestSignatureIsVerifiedOnTheCapabilityPath(t *testing.T) {
	useTestKey(t)

	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	if err := db.CreateTenant(ctx, &store.Tenant{
		ID: "tenant-a", Slug: "tenant-a", Name: "a", Plan: "beta",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	const imei = "300234065000009"
	if err := db.CreateDevice(ctx, "tenant-a", &store.Device{IMEI: imei, Label: "a"}); err != nil {
		t.Fatalf("device: %v", err)
	}

	h := NewHandler(nil, "")
	h.SetTenants(tenancy.NewResolver(db, store.DefaultTenantID, time.Minute))
	h.SetAccounts(integrations.New(db, make([]byte, 32)))
	h.SetStore(db)

	form := func() url.Values {
		return url.Values{
			"imei": {imei}, "momsn": {"7"}, "data": {"68656c6c6f"},
			"transmit_time": {"26-09-20 10:00:00"}, "serial": {"12345"},
		}
	}
	claims := func() jwt.MapClaims {
		return jwt.MapClaims{
			"imei": imei, "momsn": 7, "data": "68656c6c6f",
			"transmit_time": "26-09-20 10:00:00", "serial": "12345",
		}
	}
	// The capability path resolves the tenant before the handler runs, which
	// is what this reproduces: the request arrives already trusted.
	post := func(f url.Values) int {
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock/"+strings.Repeat("a", 32),
			strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(webhookroute.NewContext(req.Context(), &webhookroute.Resolved{TenantID: "tenant-a"}))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	t.Run("a valid signature is accepted", func(t *testing.T) {
		f := form()
		f.Set("JWT", signedJWT(t, claims()))
		if got := post(f); got != http.StatusOK {
			t.Fatalf("signed delivery: got %d, want 200", got)
		}
	})

	t.Run("a tampered payload is refused", func(t *testing.T) {
		f := form()
		f.Set("JWT", signedJWT(t, claims()))
		// Same token, different body underneath it.
		f.Set("data", "6465616462656566")
		if got := post(f); got != http.StatusUnauthorized {
			t.Fatalf("tampered payload: got %d, want 401 -- the capability path is not verifying the signature", got)
		}
	})

	t.Run("a token signed by the wrong key is refused", func(t *testing.T) {
		other, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("key: %v", err)
		}
		tok, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims()).SignedString(other)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		f := form()
		f.Set("JWT", tok)
		if got := post(f); got != http.StatusUnauthorized {
			t.Fatalf("foreign key: got %d, want 401", got)
		}
	})

	t.Run("an unsigned delivery is accepted by default", func(t *testing.T) {
		// Deliberate: refusing would drop a real MO, and an MO can be an SOS.
		if got := post(form()); got != http.StatusOK {
			t.Fatalf("unsigned delivery: got %d, want 200", got)
		}
	})

	t.Run("and is refused when the switch is on", func(t *testing.T) {
		h.SetRequireSignature(true)
		defer h.SetRequireSignature(false)
		if got := post(form()); got != http.StatusUnauthorized {
			t.Fatalf("unsigned delivery with require-signature: got %d, want 401", got)
		}
	})

	t.Run("the switch never blocks a signed delivery", func(t *testing.T) {
		h.SetRequireSignature(true)
		defer h.SetRequireSignature(false)
		f := form()
		f.Set("JWT", signedJWT(t, claims()))
		if got := post(f); got != http.StatusOK {
			t.Fatalf("signed delivery with require-signature: got %d, want 200", got)
		}
	})
}
