// Package email provides PGP-encrypted email send/receive for MeshSat Hub.
// Uses github.com/ProtonMail/go-crypto/openpgp for PGP operations.
package email

import (
	"bytes"
	"crypto"
	_ "crypto/sha256" // register SHA-256 hash
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	_ "golang.org/x/crypto/ripemd160" //nolint:staticcheck // required by openpgp for hash registration
)

// KeyRing manages PGP keys for the Hub and per-contact recipients.
type KeyRing struct {
	mu         sync.RWMutex
	hubEntity  *openpgp.Entity            // Hub's own keypair (sign + decrypt)
	contacts   map[string]*openpgp.Entity // email → recipient public key (encrypt to)
	hubArmored string                     // cached ASCII-armored Hub public key
}

// NewKeyRing creates a new PGP key ring. If hubKeyArmored is empty, generates a new keypair.
func NewKeyRing(hubName, hubEmail, hubKeyArmored string) (*KeyRing, error) {
	kr := &KeyRing{
		contacts: make(map[string]*openpgp.Entity),
	}

	if hubKeyArmored != "" {
		// Load existing Hub key.
		entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(hubKeyArmored))
		if err != nil {
			return nil, fmt.Errorf("email: read hub key: %w", err)
		}
		if len(entities) == 0 {
			return nil, fmt.Errorf("email: hub key contains no entities")
		}
		kr.hubEntity = entities[0]
	} else {
		// Generate new Hub keypair.
		config := &packet.Config{
			DefaultHash:            crypto.SHA256,
			DefaultCipher:          packet.CipherAES256,
			DefaultCompressionAlgo: packet.CompressionZLIB,
		}
		entity, err := openpgp.NewEntity(hubName, "MeshSat Hub", hubEmail, config)
		if err != nil {
			return nil, fmt.Errorf("email: generate hub key: %w", err)
		}
		kr.hubEntity = entity
	}

	// Cache the armored public key.
	pub, err := kr.armorPublicKey(kr.hubEntity)
	if err != nil {
		return nil, fmt.Errorf("email: armor hub public key: %w", err)
	}
	kr.hubArmored = pub

	return kr, nil
}

// HubPublicKey returns the Hub's PGP public key in ASCII-armored format.
func (kr *KeyRing) HubPublicKey() string {
	return kr.hubArmored
}

// AddContact stores a recipient's PGP public key (ASCII-armored).
func (kr *KeyRing) AddContact(email, armoredPubKey string) error {
	entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armoredPubKey))
	if err != nil {
		return fmt.Errorf("email: read contact key: %w", err)
	}
	if len(entities) == 0 {
		return fmt.Errorf("email: contact key contains no entities")
	}

	kr.mu.Lock()
	defer kr.mu.Unlock()
	kr.contacts[email] = entities[0]
	return nil
}

// RemoveContact removes a contact's PGP public key.
func (kr *KeyRing) RemoveContact(email string) {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	delete(kr.contacts, email)
}

// GetContact returns a contact's PGP entity, or nil if not found.
func (kr *KeyRing) GetContact(email string) *openpgp.Entity {
	kr.mu.RLock()
	defer kr.mu.RUnlock()
	return kr.contacts[email]
}

// ListContacts returns all stored contact emails.
func (kr *KeyRing) ListContacts() []string {
	kr.mu.RLock()
	defer kr.mu.RUnlock()
	emails := make([]string, 0, len(kr.contacts))
	for e := range kr.contacts {
		emails = append(emails, e)
	}
	return emails
}

// Encrypt encrypts and signs a message for the given recipient.
// If the recipient has no PGP key, returns the plaintext unchanged with encrypted=false.
func (kr *KeyRing) Encrypt(recipientEmail, plaintext string) (string, bool, error) {
	recipient := kr.GetContact(recipientEmail)
	if recipient == nil {
		return plaintext, false, nil // cleartext fallback
	}

	var buf bytes.Buffer
	armorWriter, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		return "", false, fmt.Errorf("email: armor encode: %w", err)
	}

	config := &packet.Config{
		DefaultHash:   crypto.SHA256,
		DefaultCipher: packet.CipherAES256,
	}

	w, err := openpgp.Encrypt(armorWriter, []*openpgp.Entity{recipient}, kr.hubEntity, nil, config)
	if err != nil {
		_ = armorWriter.Close()
		return "", false, fmt.Errorf("email: encrypt: %w", err)
	}

	if _, err := io.WriteString(w, plaintext); err != nil {
		_ = w.Close()
		_ = armorWriter.Close()
		return "", false, fmt.Errorf("email: write plaintext: %w", err)
	}
	_ = w.Close()
	_ = armorWriter.Close()

	return buf.String(), true, nil
}

// Decrypt decrypts a PGP-encrypted message using the Hub's private key.
// Returns the plaintext and the signer email (if signed), or error.
func (kr *KeyRing) Decrypt(ciphertext string) (plaintext, signerEmail string, err error) {
	block, err := armor.Decode(strings.NewReader(ciphertext))
	if err != nil {
		return "", "", fmt.Errorf("email: armor decode: %w", err)
	}

	// Build entity list: Hub key + all contacts (for signature verification).
	kr.mu.RLock()
	keyring := openpgp.EntityList{kr.hubEntity}
	for _, e := range kr.contacts {
		keyring = append(keyring, e)
	}
	kr.mu.RUnlock()

	md, err := openpgp.ReadMessage(block.Body, keyring, nil, nil)
	if err != nil {
		return "", "", fmt.Errorf("email: decrypt: %w", err)
	}

	body, err := io.ReadAll(md.UnverifiedBody)
	if err != nil {
		return "", "", fmt.Errorf("email: read decrypted body: %w", err)
	}

	if md.SignedBy != nil {
		for id := range md.SignedBy.Entity.Identities {
			signerEmail = id
			break
		}
	}

	return string(body), signerEmail, nil
}

// ContactInfo holds metadata about a stored PGP contact.
type ContactInfo struct {
	Email     string `json:"email"`
	Name      string `json:"name,omitempty"`
	KeyID     string `json:"key_id"`
	CreatedAt string `json:"created_at,omitempty"`
}

// ListContactInfo returns metadata for all stored contacts.
func (kr *KeyRing) ListContactInfo() []ContactInfo {
	kr.mu.RLock()
	defer kr.mu.RUnlock()
	infos := make([]ContactInfo, 0, len(kr.contacts))
	for email, entity := range kr.contacts {
		info := ContactInfo{Email: email}
		if entity.PrimaryKey != nil {
			info.KeyID = fmt.Sprintf("%X", entity.PrimaryKey.KeyId)
			info.CreatedAt = entity.PrimaryKey.CreationTime.Format(time.RFC3339)
		}
		for id := range entity.Identities {
			info.Name = id
			break
		}
		infos = append(infos, info)
	}
	return infos
}

func (kr *KeyRing) armorPublicKey(entity *openpgp.Entity) (string, error) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		return "", err
	}
	if err := entity.Serialize(w); err != nil {
		_ = w.Close()
		return "", err
	}
	_ = w.Close()
	return buf.String(), nil
}
