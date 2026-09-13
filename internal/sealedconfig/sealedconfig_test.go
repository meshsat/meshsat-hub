package sealedconfig

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
)

type memStore struct {
	rows map[string]string
	sets int
}

func newMem() *memStore { return &memStore{rows: map[string]string{}} }

func (m *memStore) GetSystemConfig(_ context.Context, key string) (string, error) {
	v, ok := m.rows[key]
	if !ok {
		return "", sql.ErrNoRows
	}
	return v, nil
}

func (m *memStore) SetSystemConfig(_ context.Context, key, value string) error {
	m.sets++
	m.rows[key] = value
	return nil
}

func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := hubcrypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return k
}

// THE TEST THAT MATTERS. Every one of the five call sites this package exists
// for responded to an unreadable value by generating a new one: the master key
// logged "bootstrapped" while every tenant's carrier credentials became
// undecryptable, and the directory anchor silently replaced the key every field
// bridge had pinned. If Get ever reports a sealed-but-unreadable value as
// "absent", those holes reopen.
func TestAnUnreadableSealedValueIsNeverReportedAsAbsent(t *testing.T) {
	ctx := context.Background()
	good, wrong := testKey(t), testKey(t)

	m := newMem()
	if err := New(m, good).Set(ctx, "bridge_ca_key", "-----BEGIN EC PRIVATE KEY-----"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Leave a plaintext row too: the fallback must NOT rescue a bad unwrap,
	// or the migration would silently serve stale material.
	m.rows["bridge_ca_key"] = "-----BEGIN EC PRIVATE KEY-----stale"

	_, err := New(m, wrong).Get(ctx, "bridge_ca_key")
	if !errors.Is(err, ErrSealed) {
		t.Fatalf("a wrong wrap key gave %v, want ErrSealed", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("reported as absent, which is the signal to GENERATE A NEW KEY -- " +
			"this is how every tenant's credentials get orphaned with a cheerful log line")
	}

	// And with no wrap key at all, a sealed row must still not fall back.
	_, err = New(m, nil).Get(ctx, "bridge_ca_key")
	if !errors.Is(err, ErrSealed) {
		t.Fatalf("no wrap key against a sealed row gave %v, want ErrSealed", err)
	}
}

func TestASealedValueRoundTrips(t *testing.T) {
	ctx := context.Background()
	k := New(newMem(), testKey(t))
	for _, v := range []string{
		"-----BEGIN EC PRIVATE KEY-----\nMHcCAQ\n-----END EC PRIVATE KEY-----\n",
		strings.Repeat("a1", 32),
		"",
	} {
		if err := k.Set(ctx, "x", v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
		got, err := k.Get(ctx, "x")
		if v == "" {
			// An empty value seals to a non-empty blob, so it comes back as
			// itself rather than as ErrNotFound. Worth pinning: a blanked
			// plaintext row and a sealed empty string are different things.
			if err != nil || got != "" {
				t.Errorf("empty value: got %q, %v", got, err)
			}
			continue
		}
		if err != nil || got != v {
			t.Errorf("round trip: got %q, %v; want %q", got, err, v)
		}
	}
}

func TestTheSealedRowWinsOverThePlaintextOne(t *testing.T) {
	ctx := context.Background()
	m := newMem()
	k := New(m, testKey(t))
	m.rows["k"] = "the old plaintext"
	if err := k.Set(ctx, "k", "the sealed one"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := k.Get(ctx, "k")
	if err != nil || got != "the sealed one" {
		t.Errorf("got %q, %v; want the sealed value to win", got, err)
	}
	if m.rows["k"] != "the old plaintext" {
		t.Error("Set mutated the plaintext row; blanking it must stay a separate, deliberate step")
	}
}

func TestWithoutASealedRowItFallsBackToPlaintext(t *testing.T) {
	ctx := context.Background()
	m := newMem()
	m.rows["k"] = "plaintext"
	got, err := New(m, testKey(t)).Get(ctx, "k")
	if err != nil || got != "plaintext" {
		t.Errorf("got %q, %v; want the plaintext fallback so a deploy migrates forward", got, err)
	}
}

func TestNothingStoredIsTheOnlySignalToGenerate(t *testing.T) {
	ctx := context.Background()
	m := newMem()
	if _, err := New(m, testKey(t)).Get(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing key gave %v, want ErrNotFound", err)
	}
	// A BLANKED plaintext row is also "nothing stored" -- that is what the
	// claim path writes, and what step five of the migration writes.
	m.rows["k"] = ""
	if _, err := New(m, testKey(t)).Get(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("blanked row gave %v, want ErrNotFound", err)
	}
}

func TestMigrateSealsOnceAndLeavesThePlaintextAlone(t *testing.T) {
	ctx := context.Background()
	m := newMem()
	m.rows["k"] = "secret"
	k := New(m, testKey(t))

	moved, err := k.Migrate(ctx, "k")
	if err != nil || !moved {
		t.Fatalf("first Migrate: moved=%v err=%v", moved, err)
	}
	if m.rows["k"] != "secret" {
		t.Error("Migrate blanked the plaintext row; that is a separate step, taken only after the sealed rows are proven")
	}
	if got, _ := k.Get(ctx, "k"); got != "secret" {
		t.Errorf("after Migrate, Get = %q", got)
	}

	before := m.sets
	moved, err = k.Migrate(ctx, "k")
	if err != nil || moved {
		t.Errorf("second Migrate: moved=%v err=%v, want a no-op", moved, err)
	}
	if m.sets != before {
		t.Error("Migrate rewrote an already-sealed row; it runs on every start and must be idempotent")
	}
}

func TestMigrateDoesNothingWithoutAWrapKey(t *testing.T) {
	ctx := context.Background()
	m := newMem()
	m.rows["k"] = "secret"
	moved, err := New(m, nil).Migrate(ctx, "k")
	if err != nil || moved {
		t.Errorf("moved=%v err=%v, want a no-op so a pod without the key changes nothing", moved, err)
	}
	if _, ok := m.rows["k"+Suffix]; ok {
		t.Error("wrote a sealed row with no wrap key")
	}
}

func TestParseWrapKeyRefusesWhatCannotBeOpenedLater(t *testing.T) {
	good := hex.EncodeToString(testKey(t))
	if _, err := ParseWrapKey("  " + good + "\n"); err != nil {
		t.Errorf("a valid key with whitespace was refused: %v", err)
	}
	for _, tc := range []struct{ what, in string }{
		{"empty", ""},
		{"whitespace only", "   "},
		{"not hex", "zzzz"},
		{"too short", hex.EncodeToString([]byte("sixteen bytes!!!"))},
		{"too long", good + "00"},
	} {
		if _, err := ParseWrapKey(tc.in); !errors.Is(err, ErrNoWrapKey) {
			t.Errorf("%s: got %v, want ErrNoWrapKey", tc.what, err)
		}
	}
}

// The "<no value>" case needs its own test, and asserting only that it is
// REFUSED would be vacuous: it is not hex, so it fails the hex decode whether
// or not the explicit check exists. Proved by deleting the check and watching
// the test stay green. What earns its keep is the MESSAGE -- this is the one
// failure an operator will actually hit, when the ExternalSecret references a
// property nobody wrote, and "encoding/hex: invalid byte" sends them looking in
// the wrong place entirely.
func TestTheMissingPropertyLiteralSaysWhatToDoAboutIt(t *testing.T) {
	_, err := ParseWrapKey("<no value>")
	if !errors.Is(err, ErrNoWrapKey) {
		t.Fatalf("got %v, want ErrNoWrapKey", err)
	}
	msg := err.Error()
	for _, want := range []string{"<no value>", "missing", "before referencing"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q, so it reads as a malformed key "+
				"rather than an unwritten secret: %s", want, msg)
		}
	}
}

func TestWithoutAWrapKeyItBehavesExactlyAsBefore(t *testing.T) {
	ctx := context.Background()
	m := newMem()
	k := New(m, nil)
	if k.Sealing() {
		t.Error("Sealing() true with no key")
	}
	if err := k.Set(ctx, "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if m.rows["k"] != "v" {
		t.Errorf("plaintext row = %q; a pod with no wrap key must keep working the old way", m.rows["k"])
	}
	if _, sealed := m.rows["k"+Suffix]; sealed {
		t.Error("wrote a sealed row with no wrap key")
	}
}

// A sealed blob must not be mistakable for the plaintext it replaced -- if it
// were, a rollback that reads the sealed row as a PEM would hand a nil CA to
// every issuance endpoint.
func TestASealedValueDoesNotLookLikeTheKeyItReplaced(t *testing.T) {
	ctx := context.Background()
	m := newMem()
	pem := "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\n-----END EC PRIVATE KEY-----\n"
	if err := New(m, testKey(t)).Set(ctx, "bridge_ca_key", pem); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got := m.rows["bridge_ca_key"+Suffix]
	if strings.Contains(got, "PRIVATE KEY") || strings.Contains(got, "BEGIN") {
		t.Error("the sealed row still carries PEM markers")
	}
	if got == pem {
		t.Fatal("the value was stored unsealed")
	}
}
