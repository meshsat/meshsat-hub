package invoiceninja

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The first real subscription payment parked with
//
//	HTTP 422 {"errors":{"id_number":["The id number has already been taken."]}}
//
// EUR 9.00 taken and no VAT document. The tenant had paid once before, as a
// donation, from a different address: ensureCustomer looked the customer up by
// EMAIL, missed, and tried to create a second record carrying the same tenant
// in id_number -- which is UNIQUE in the billing system.
//
// So any tenant paying from two addresses got a permanently blocked receipt.
// A person changing their billing email, paying with a company card, or the
// platform tenant testing a donation and then a subscription all hit it.
//
// ninjaDouble is a deliberately faithful double: it filters clients by email
// AND by id_number, and it enforces the uniqueness that caused this.
type ninjaDouble struct {
	mu      sync.Mutex
	clients []map[string]string // id, id_number, email
	creates int
	nextID  int
}

func (n *ninjaDouble) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/clients", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodGet {
			email, ref := r.URL.Query().Get("email"), r.URL.Query().Get("id_number")
			out := []map[string]string{}
			for _, c := range n.clients {
				switch {
				case email != "" && strings.EqualFold(c["email"], email),
					ref != "" && c["id_number"] != "" && strings.EqualFold(c["id_number"], ref):
					out = append(out, c)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
			return
		}

		n.creates++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		ref, _ := body["id_number"].(string)
		for _, c := range n.clients {
			// Uniqueness binds a real reference, not an absent one.
			if ref != "" && strings.EqualFold(c["id_number"], ref) {
				// What the real billing system answers. id_number is UNIQUE.
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"message":"The given data was invalid.",` +
					`"errors":{"id_number":["The id number has already been taken."]}}`))
				return
			}
		}
		n.nextID++
		email := ""
		if cs, ok := body["contacts"].([]any); ok && len(cs) > 0 {
			if m, ok := cs[0].(map[string]any); ok {
				email, _ = m["email"].(string)
			}
		}
		id := "client-" + string(rune('a'+n.nextID-1))
		n.clients = append(n.clients, map[string]string{"id": id, "id_number": ref, "email": email})
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"id": id}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func tenantReq(email string) Request {
	return Request{
		CustomerRef: "default", Name: "A Customer", Email: email,
		AmountCents: 900, Currency: "EUR", ProductKey: "Crew",
		Description: "one month", PaidAt: time.Now().UTC(),
	}
}

func TestASecondAddressReusesTheTenantsCustomerRecord(t *testing.T) {
	n := &ninjaDouble{}
	c := New(n.server(t).URL, "test-token", 5*time.Second)

	// First payment, one address. Creates the record.
	first, err := c.ensureCustomer(context.Background(), tenantReq("kyriakos@example.org"))
	if err != nil {
		t.Fatalf("first payment: %v", err)
	}

	// Second payment, SAME tenant, different address. This is the case that
	// took the money and issued nothing.
	second, err := c.ensureCustomer(context.Background(), tenantReq("test@example.org"))
	if err != nil {
		t.Fatalf("second payment from another address: %v; the receipt would park "+
			"and the money would have no document", err)
	}
	if second != first {
		t.Errorf("got customer %q, want the tenant's existing %q", second, first)
	}
	if n.creates != 1 {
		t.Errorf("attempted %d creates; the second was refused by the billing system", n.creates)
	}
	if len(n.clients) != 1 {
		t.Errorf("%d customer records for one tenant", len(n.clients))
	}
}

// Email still wins when it matches: it is the more specific identity, and two
// tenants must never collapse into one record.
func TestADifferentTenantStillGetsItsOwnRecord(t *testing.T) {
	n := &ninjaDouble{}
	c := New(n.server(t).URL, "test-token", 5*time.Second)

	a, err := c.ensureCustomer(context.Background(), tenantReq("a@example.org"))
	if err != nil {
		t.Fatal(err)
	}
	other := tenantReq("b@example.org")
	other.CustomerRef = "t_other"
	b, err := c.ensureCustomer(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two tenants share one customer record; their documents would mix")
	}
	if len(n.clients) != 2 {
		t.Errorf("%d records, want one per tenant", len(n.clients))
	}
}

// The billing system matches id_number case-insensitively. A record is where
// money is filed, so the match is confirmed rather than assumed.
func TestANearMissOnTheTenantIsNotReused(t *testing.T) {
	n := &ninjaDouble{}
	n.clients = append(n.clients, map[string]string{
		"id": "client-z", "id_number": "default-but-not-really", "email": "z@example.org"})
	c := New(n.server(t).URL, "test-token", 5*time.Second)

	id, err := c.ensureCustomer(context.Background(), tenantReq("new@example.org"))
	if err != nil {
		t.Fatal(err)
	}
	if id == "client-z" {
		t.Fatal("a different tenant's record was reused")
	}
}

// A request with no tenant reference must not match the first record it sees.
func TestNoTenantReferenceCreatesRatherThanGuesses(t *testing.T) {
	n := &ninjaDouble{}
	n.clients = append(n.clients, map[string]string{
		"id": "client-z", "id_number": "", "email": "someone@example.org"})
	c := New(n.server(t).URL, "test-token", 5*time.Second)

	req := tenantReq("fresh@example.org")
	req.CustomerRef = ""
	id, err := c.ensureCustomer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if id == "client-z" {
		t.Fatal("an unrelated record was reused for a request carrying no tenant")
	}
}
