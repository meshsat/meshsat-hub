package sms

import (
	"context"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

func newWAAccounts(t *testing.T) *integrations.Service {
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
		key[i] = byte(i * 5)
	}
	return integrations.New(db, key)
}

// MESHSAT-1367. WhatsApp is registered on its own number in Twilio, so a
// tenant's WhatsApp client sends From whatsapp_from when it is set and From
// the SMS number only when it is not; the platform's own client is what the
// default tenant gets, and a tenant with no account gets nothing, never the
// platform's sender.
func TestWhatsAppForTenantUsesTheAccountsWhatsAppSender(t *testing.T) {
	ctx := context.Background()
	accounts := newWAAccounts(t)
	if _, err := accounts.Set(ctx, "t_wa", integrations.ProviderTwilio,
		map[string]string{"account_sid": "ACwa", "auth_token": "tok", "from_number": "+3197010000000", "whatsapp_from": "+3197006000000"}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Set(ctx, "t_shared", integrations.ProviderTwilio,
		map[string]string{"account_sid": "ACsh", "auth_token": "tok", "from_number": "+3197010000001"}); err != nil {
		t.Fatal(err)
	}
	// The platform's own Twilio account is what main.go registers from the
	// environment; without it the default tenant has no account at all.
	accounts.SetPlatform(integrations.ProviderTwilio, map[string]string{"account_sid": "ACplat", "auth_token": "tok", "from_number": "+3197010258258", "whatsapp_from": "+3197006531641"})
	platformSMS := NewClient("ACplat", "tok", "+3197010258258")
	platformWA := NewClient("ACplat", "tok", "+3197006531641")
	platformWA.SetChannel("whatsapp")
	pool := NewClientPool(platformSMS, accounts)
	pool.SetPlatformWhatsApp(platformWA)

	c := pool.WhatsAppForTenant(ctx, "t_wa")
	if c == nil || c.fromNumber != "+3197006000000" || c.channel != "whatsapp" {
		t.Fatalf("tenant with a WhatsApp sender: %+v", c)
	}
	if got := c.addr(c.fromNumber); got != "whatsapp:+3197006000000" {
		t.Errorf("From address = %q, want the whatsapp: prefix on the WhatsApp sender", got)
	}
	c = pool.WhatsAppForTenant(ctx, "t_shared")
	if c == nil || c.fromNumber != "+3197010000001" || c.channel != "whatsapp" {
		t.Fatalf("tenant without a WhatsApp sender falls back to its SMS number: %+v", c)
	}
	if pool.WhatsAppForTenant(ctx, store.DefaultTenantID) != platformWA {
		t.Errorf("the default tenant does not get the platform's WhatsApp client")
	}
	if pool.WhatsAppForTenant(ctx, "t_none") != nil {
		t.Errorf("a tenant with no Twilio account was handed a WhatsApp client (the platform's sender would be billed for a customer's traffic)")
	}
	// The SMS client of the same tenant is untouched by the WhatsApp field.
	if s := pool.ForTenant(ctx, "t_wa"); s == nil || s.fromNumber != "+3197010000000" || s.channel == "whatsapp" {
		t.Errorf("SMS client changed by the WhatsApp sender: %+v", s)
	}
}
