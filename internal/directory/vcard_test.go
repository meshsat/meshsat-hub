package directory

import (
	"bytes"
	"strings"
	"testing"
)

func TestVCard_RoundTrip(t *testing.T) {
	alice := Contact{
		ID:          "00000000-0000-4000-8000-000000000001",
		TenantID:    "acme",
		DisplayName: "Alice Kowalski",
		GivenName:   "Alice",
		FamilyName:  "Kowalski",
		Org:         "Meshsat Rescue",
		Role:        "Medic",
		Team:        "Red",
		SIDC:        "SFGPUCI----I",
		Notes:       "Primary SAR contact",
		TrustLevel:  TrustInPerson,
		Addresses: []Address{
			{Kind: KindSMS, Value: "+31612345678"},
			{Kind: KindMeshtastic, Value: "!abcd1234"},
			{Kind: KindAPRS, Value: "PA1A-9"},
			{Kind: KindIridiumSBD, Value: "300234012345670"},
			{Kind: KindTAK, Value: "ALICE.MEDIC"},
			{Kind: KindReticulum, Value: "abcdef0123456789"},
			{Kind: KindEmail, Value: "alice@example.com"},
		},
	}

	var buf bytes.Buffer
	if err := WriteVCards(&buf, []Contact{alice}); err != nil {
		t.Fatalf("WriteVCards: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"BEGIN:VCARD", "VERSION:4.0", "FN:Alice Kowalski",
		"X-MESHSAT-SIDC:SFGPUCI----I", "X-MESHSAT-TRUST-LEVEL:3",
		"X-MESHSAT-MESHTASTIC:!abcd1234", "X-MESHSAT-TAK:ALICE.MEDIC",
		"X-MESHSAT-IRIDIUM-SBD:300234012345670", "TEL;TYPE=cell:+31612345678",
		"EMAIL:alice@example.com", "END:VCARD",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	parsed, err := ParseVCards(strings.NewReader(out))
	if err != nil {
		t.Fatalf("ParseVCards: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("count: %d", len(parsed))
	}
	bob := parsed[0]
	if bob.DisplayName != alice.DisplayName || bob.Team != "Red" ||
		bob.Role != "Medic" || bob.SIDC != "SFGPUCI----I" ||
		bob.TrustLevel != TrustInPerson || bob.ID != alice.ID {
		t.Errorf("core fields not preserved: %+v", bob)
	}
	wantKinds := map[AddressKind]string{
		KindSMS:        "+31612345678",
		KindMeshtastic: "!abcd1234",
		KindAPRS:       "PA1A-9",
		KindIridiumSBD: "300234012345670",
		KindTAK:        "ALICE.MEDIC",
		KindReticulum:  "abcdef0123456789",
		KindEmail:      "alice@example.com",
	}
	for kind, want := range wantKinds {
		found := false
		for _, a := range bob.Addresses {
			if a.Kind == kind && a.Value == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("kind %s value %q missing in %+v", kind, want, bob.Addresses)
		}
	}
}

func TestVCard_EscapingRoundTrip(t *testing.T) {
	c := Contact{
		DisplayName: "Note, with; odd\\chars",
		Notes:       "Line 1\nLine 2",
		Addresses: []Address{
			{Kind: KindEmail, Value: "edge@example.com"},
		},
	}
	var buf bytes.Buffer
	if err := WriteVCards(&buf, []Contact{c}); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseVCards(&buf)
	if err != nil {
		t.Fatal(err)
	}
	got := parsed[0]
	if got.DisplayName != c.DisplayName {
		t.Errorf("DisplayName: %q vs %q", got.DisplayName, c.DisplayName)
	}
	if got.Notes != c.Notes {
		t.Errorf("Notes: %q vs %q", got.Notes, c.Notes)
	}
}

func TestVCard_Multiple(t *testing.T) {
	input := "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:First\r\nEND:VCARD\r\nBEGIN:VCARD\r\nVERSION:4.0\r\nFN:Second\r\nEND:VCARD\r\n"
	parsed, err := ParseVCards(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 2 || parsed[0].DisplayName != "First" || parsed[1].DisplayName != "Second" {
		t.Errorf("parse: %+v", parsed)
	}
}

func TestVCard_MissingFN(t *testing.T) {
	_, err := ParseVCards(strings.NewReader("BEGIN:VCARD\r\nNOTE:orphan\r\nEND:VCARD\r\n"))
	if err == nil {
		t.Error("expected error for FN-less vCard")
	}
}
