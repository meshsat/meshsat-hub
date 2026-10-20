package directory

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fixtureKey struct {
	KeyID   string `json:"key_id"`
	PKIXB64 string `json:"pkix_b64"`
}

// TestEnvelopeFixture runs every case of the pinned C7 fixture (fixtures
// v1.1, directory_v2.json): the Hub's verifier agrees with the reference on
// each, including the tampered payload and the key trust of a rotation.
func TestEnvelopeFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "v1", "directory_v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Keys  map[string]fixtureKey `json:"keys"`
		Cases []struct {
			Case         string   `json:"case"`
			TrustedKey   string   `json:"trusted_key"`
			Envelope     Envelope `json:"envelope"`
			Verify       bool     `json:"verify"`
			ApplyVersion int64    `json:"apply_version"`
			RotatesTo    string   `json:"rotates_to"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) != 6 {
		t.Fatalf("%d cases, the release has 6", len(fx.Cases))
	}
	for _, c := range fx.Cases {
		k := fx.Keys[c.TrustedKey]
		der, err := base64.StdEncoding.DecodeString(k.PKIXB64)
		if err != nil || KeyID(der) != k.KeyID {
			t.Fatalf("%s: fixture key %s does not hash to its id", c.Case, c.TrustedKey)
		}
		payload, err := OpenEnvelope(&c.Envelope, func(id string) []byte {
			if id == k.KeyID {
				return der
			}
			return nil
		})
		if (err == nil) != c.Verify {
			t.Errorf("%s: verified=%v (%v), fixture says %v", c.Case, err == nil, err, c.Verify)
			continue
		}
		if err != nil {
			continue
		}
		var head struct {
			Type      string `json:"type"`
			Version   int64  `json:"version"`
			NewKeyID  string `json:"new_key_id"`
			NewKeyB64 string `json:"new_key_pkix"`
		}
		if err := json.Unmarshal(payload, &head); err != nil {
			t.Fatalf("%s: payload: %v", c.Case, err)
		}
		if c.ApplyVersion != 0 && (head.Version != c.ApplyVersion || (head.Type != "" && head.Type != PayloadSnapshot)) {
			t.Errorf("%s: snapshot type %q version %d, want %d", c.Case, head.Type, head.Version, c.ApplyVersion)
		}
		if c.RotatesTo != "" {
			to := fx.Keys[c.RotatesTo]
			if head.Type != PayloadRotation || head.NewKeyID != to.KeyID || head.NewKeyB64 != to.PKIXB64 {
				t.Errorf("%s: rotation names %q, want %s", c.Case, head.NewKeyID, to.KeyID)
			}
		}
	}
}

func testAnchor(t *testing.T) *TrustAnchor {
	t.Helper()
	a, err := LoadOrCreateTrustAnchor(context.Background(), newFakeStore())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func trustOnly(a *TrustAnchor) func(string) []byte {
	der := a.PublicKey()
	return func(id string) []byte {
		if id == KeyID(der) {
			return der
		}
		return nil
	}
}

func TestSealOpenAndWhatBreaksIt(t *testing.T) {
	a, other := testAnchor(t), testAnchor(t)
	payload := []byte(`{"type":"snapshot","tenant_id":"t","version":3,"x_new":{"a":1}}`)
	env, err := a.Seal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if env.V != 2 || env.Alg != "ES256" || env.KeyID != KeyID(a.PublicKey()) {
		t.Fatalf("envelope header %+v", env)
	}
	wire, _ := json.Marshal(env)
	var back Envelope
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatal(err)
	}
	got, err := OpenEnvelope(&back, trustOnly(a))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("round trip: %q %v", got, err)
	}
	mutate := map[string]func(e *Envelope){
		"payload changed": func(e *Envelope) {
			e.Payload = bytes.Replace(e.Payload, []byte(`"version":3`), []byte(`"version":4`), 1)
		},
		"signature cut":    func(e *Envelope) { e.Sig = e.Sig[:len(e.Sig)-1] },
		"version 1":        func(e *Envelope) { e.V = 1 },
		"another alg":      func(e *Envelope) { e.Alg = "EdDSA" },
		"no signature":     func(e *Envelope) { e.Sig = nil },
		"another key's id": func(e *Envelope) { e.KeyID = KeyID(other.PublicKey()) },
	}
	for name, m := range mutate {
		e := back
		e.Payload = append([]byte(nil), back.Payload...)
		e.Sig = append([]byte(nil), back.Sig...)
		m(&e)
		if _, err := OpenEnvelope(&e, trustOnly(a)); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	// Signed by a key that is not trusted.
	foreign, _ := other.Seal(payload)
	if _, err := OpenEnvelope(foreign, trustOnly(a)); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("foreign key: %v", err)
	}
	// A trust lookup that answers with the wrong key for the id.
	if _, err := OpenEnvelope(env, func(string) []byte { return other.PublicKey() }); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("mismatched key for the id: %v", err)
	}
}

// TestSnapshotPayloadCarriesOriginAndSeeded: every row of a v2 payload
// names its origin hub:<tenant>, and each policy its seeded mark, inside
// the signed bytes (C7.3, C7.7).
func TestSnapshotPayloadCarriesOriginAndSeeded(t *testing.T) {
	a := testAnchor(t)
	snap := &Snapshot{TenantID: "t-1", Version: 9, SignedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
		Contacts: []Contact{{ID: "c", TenantID: "t-1", DisplayName: "C", Origin: OriginImported,
			Addresses: []Address{{Kind: KindTETRA, Value: "tetra:1001"}}}},
		Groups: []Group{{ID: "g", TenantID: "t-1", Name: "G", Kind: GroupTeam}},
		Policies: []DispatchPolicy{
			{ID: "p1", TenantID: "t-1", ScopeType: ScopePrecedence, ScopeID: "Flash", Strategy: StrategyHeMBBonded, Seeded: true},
			{ID: "p2", TenantID: "t-1", ScopeType: ScopeDefault, Strategy: StrategyPrimaryOnly},
		}}
	env, err := a.SealSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := OpenEnvelope(env, trustOnly(a))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Type     string `json:"type"`
		TenantID string `json:"tenant_id"`
		Version  int64  `json:"version"`
		Contacts []struct {
			Origin    string `json:"origin"`
			Addresses []struct {
				Origin, Value string
			} `json:"addresses"`
		} `json:"contacts"`
		Groups []struct {
			Origin    string   `json:"origin"`
			Kind      string   `json:"kind"`
			MemberIDs []string `json:"member_ids"`
		} `json:"groups"`
		Policies []map[string]any `json:"policies"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != PayloadSnapshot || p.TenantID != "t-1" || p.Version != 9 {
		t.Errorf("head: %+v", p)
	}
	if p.Contacts[0].Origin != "hub:t-1" || p.Contacts[0].Addresses[0].Origin != "hub:t-1" || p.Contacts[0].Addresses[0].Value != "tetra:1001" {
		t.Errorf("contact origin: %+v", p.Contacts[0])
	}
	if p.Groups[0].Origin != "hub:t-1" || p.Groups[0].Kind != "TEAM" || p.Groups[0].MemberIDs == nil {
		t.Errorf("group: %+v", p.Groups[0])
	}
	if p.Policies[0]["seeded"] != true || p.Policies[1]["seeded"] != false || p.Policies[0]["origin"] != "hub:t-1" {
		t.Errorf("policies: %+v", p.Policies)
	}
	if _, ok := p.Policies[0]["TenantID"]; ok {
		t.Error("a policy carries its Go field name")
	}
}
