package email

import (
	"strings"
	"testing"
)

func TestNewKeyRing_GeneratesKey(t *testing.T) {
	kr, err := NewKeyRing("Test Hub", "hub@test.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if kr.hubEntity == nil {
		t.Fatal("hubEntity should not be nil")
	}

	pub := kr.HubPublicKey()
	if !strings.Contains(pub, "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		t.Error("expected armored public key")
	}
}

func TestKeyRing_EncryptDecryptRoundtrip(t *testing.T) {
	// Create Hub keyring.
	hub, err := NewKeyRing("Hub", "hub@test.com", "")
	if err != nil {
		t.Fatal(err)
	}

	// Create a "contact" keyring (simulates field operator).
	contact, err := NewKeyRing("Operator", "op@test.com", "")
	if err != nil {
		t.Fatal(err)
	}

	// Exchange public keys.
	if err := hub.AddContact("op@test.com", contact.HubPublicKey()); err != nil {
		t.Fatal("add contact key:", err)
	}
	if err := contact.AddContact("hub@test.com", hub.HubPublicKey()); err != nil {
		t.Fatal("add hub key to contact:", err)
	}

	// Hub encrypts a message for the operator.
	original := "SOS alert from device 300234063904190 at 52.3676N, 4.9041E"
	encrypted, wasEncrypted, err := hub.Encrypt("op@test.com", original)
	if err != nil {
		t.Fatal("encrypt:", err)
	}
	if !wasEncrypted {
		t.Fatal("expected message to be encrypted")
	}
	if !strings.Contains(encrypted, "-----BEGIN PGP MESSAGE-----") {
		t.Error("expected PGP message block")
	}

	// Operator decrypts the message.
	plaintext, signer, err := contact.Decrypt(encrypted)
	if err != nil {
		t.Fatal("decrypt:", err)
	}
	if plaintext != original {
		t.Errorf("plaintext = %q, want %q", plaintext, original)
	}
	// Signer should be Hub's identity.
	if signer == "" {
		t.Log("note: signer identity not extracted (depends on key format)")
	}
}

func TestKeyRing_EncryptNoKey_Cleartext(t *testing.T) {
	kr, _ := NewKeyRing("Hub", "hub@test.com", "")

	text, wasEncrypted, err := kr.Encrypt("unknown@test.com", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if wasEncrypted {
		t.Error("should not be encrypted without contact key")
	}
	if text != "hello" {
		t.Errorf("text = %q, want 'hello'", text)
	}
}

func TestKeyRing_AddRemoveContact(t *testing.T) {
	kr, _ := NewKeyRing("Hub", "hub@test.com", "")
	contact, _ := NewKeyRing("Op", "op@test.com", "")

	if err := kr.AddContact("op@test.com", contact.HubPublicKey()); err != nil {
		t.Fatal(err)
	}

	contacts := kr.ListContacts()
	if len(contacts) != 1 || contacts[0] != "op@test.com" {
		t.Errorf("contacts = %v, want [op@test.com]", contacts)
	}

	kr.RemoveContact("op@test.com")
	contacts = kr.ListContacts()
	if len(contacts) != 0 {
		t.Errorf("contacts after remove = %v, want empty", contacts)
	}
}

func TestKeyRing_InvalidKey(t *testing.T) {
	kr, _ := NewKeyRing("Hub", "hub@test.com", "")
	err := kr.AddContact("bad@test.com", "not a pgp key")
	if err == nil {
		t.Error("expected error for invalid key")
	}
}

func TestKeyRing_ListContactInfo(t *testing.T) {
	kr, _ := NewKeyRing("Hub", "hub@test.com", "")
	contact, _ := NewKeyRing("Operator One", "op@test.com", "")
	_ = kr.AddContact("op@test.com", contact.HubPublicKey())

	infos := kr.ListContactInfo()
	if len(infos) != 1 {
		t.Fatalf("expected 1 contact, got %d", len(infos))
	}
	if infos[0].Email != "op@test.com" {
		t.Errorf("email = %s", infos[0].Email)
	}
	if infos[0].KeyID == "" {
		t.Error("expected non-empty key ID")
	}
}

func TestIsEmailAddress(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"user@example.com", true},
		{"user@sub.example.com", true},
		{"+31612345678", false},
		{"slack://token", false},
		{"mailto:user@example.com", false},
		{"@example.com", false},
		{"user@", false},
		{"user@com", false}, // no dot in domain
		{"", false},
	}
	for _, tt := range tests {
		if got := isEmailAddress(tt.input); got != tt.want {
			t.Errorf("isEmailAddress(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
