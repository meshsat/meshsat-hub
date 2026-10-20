package directory

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The v2 directory envelope (contract C7.2, fixture docs/fixtures/v1/
// directory_v2.json): the signature covers the exact payload bytes, and a
// verifier checks it before it parses them, so a field it does not know is
// still covered by the signature and ignored after verification. A v1
// snapshot was re-encoded by the verifier, so any field the Bridge did not
// know broke the signature. [MESHSAT-1584 C7]
const (
	EnvelopeVersion = 2
	EnvelopeAlg     = "ES256"

	// PayloadSnapshot and PayloadRotation are the payload's "type". The
	// fixture's snapshot payload has none, so a verifier reads a missing
	// type as a snapshot; a rotation record always names its type.
	PayloadSnapshot = "snapshot"
	PayloadRotation = "rotation"
)

// Envelope is the signed wrapper. Payload and Sig are base64 on the wire
// (encoding/json's []byte form).
type Envelope struct {
	V       int    `json:"v"`
	KeyID   string `json:"key_id"`
	Alg     string `json:"alg"`
	Payload []byte `json:"payload"`
	Sig     []byte `json:"sig"`
}

// Errors from OpenEnvelope.
var (
	ErrEnvelopeFormat = errors.New("directory: not a v2 ES256 envelope")
	ErrUnknownKey     = errors.New("directory: envelope signed by a key that is not trusted")
	ErrBadSignature   = errors.New("directory: envelope signature does not verify")
)

// KeyID is the key's name in an envelope: SHA-256 of its PKIX DER, hex.
func KeyID(pkix []byte) string {
	sum := sha256.Sum256(pkix)
	return hex.EncodeToString(sum[:])
}

// Seal signs payload with the anchor's current key. The key and its id are
// read under one lock, so a rotation in between cannot pair one key's id
// with the other's signature.
func (t *TrustAnchor) Seal(payload []byte) (*Envelope, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	digest := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, t.key, digest[:])
	if err != nil {
		return nil, fmt.Errorf("directory: seal: %w", err)
	}
	out := make([]byte, len(payload))
	copy(out, payload)
	return &Envelope{V: EnvelopeVersion, KeyID: KeyID(t.pub), Alg: EnvelopeAlg, Payload: out, Sig: sig}, nil
}

// OpenEnvelope verifies env and returns its payload bytes, unparsed.
// trusted returns the PKIX DER of a trusted key by id, or nil. Nothing of
// the payload is read before the signature verifies.
func OpenEnvelope(env *Envelope, trusted func(keyID string) []byte) ([]byte, error) {
	if env == nil || env.V != EnvelopeVersion || env.Alg != EnvelopeAlg || len(env.Sig) == 0 {
		return nil, ErrEnvelopeFormat
	}
	der := trusted(env.KeyID)
	if der == nil || KeyID(der) != env.KeyID {
		return nil, fmt.Errorf("%w: %s", ErrUnknownKey, env.KeyID)
	}
	pubAny, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnknownKey, err)
	}
	pub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: not a P-256 key", ErrUnknownKey)
	}
	digest := sha256.Sum256(env.Payload)
	if !ecdsa.VerifyASN1(pub, digest[:], env.Sig) {
		return nil, ErrBadSignature
	}
	return env.Payload, nil
}

// HubOrigin is the ownership a Bridge records for what a tenant's Hub sent
// (C7.3): a full snapshot replaces only rows of this origin.
func HubOrigin(tenantID string) string { return "hub:" + tenantID }

// The v2 payload rows: the REST shapes, each with its origin, and a
// policy's seeded mark (C7.7), which the v1 snapshot did not carry. The
// outer fields shadow the embedded ones of the same JSON name.
type (
	wireAddress struct {
		Address
		Origin string `json:"origin"`
	}
	wireContact struct {
		Contact
		Origin    string        `json:"origin"`
		Addresses []wireAddress `json:"addresses"`
	}
	wireGroup struct {
		Group
		Origin string `json:"origin"`
	}
	wirePolicy struct {
		DispatchPolicy
		Seeded bool   `json:"seeded"`
		Origin string `json:"origin"`
	}
)

// SnapshotPayloadV2 is what a v2 snapshot envelope signs.
type SnapshotPayloadV2 struct {
	Type     string        `json:"type"`
	TenantID string        `json:"tenant_id"`
	Version  int64         `json:"version"`
	SignedAt time.Time     `json:"signed_at"`
	Contacts []wireContact `json:"contacts"`
	Groups   []wireGroup   `json:"groups"`
	Policies []wirePolicy  `json:"policies"`
}

// SnapshotPayload encodes snap as the v2 payload bytes.
func SnapshotPayload(snap *Snapshot) ([]byte, error) {
	if snap == nil || snap.TenantID == "" {
		return nil, fmt.Errorf("%w: snapshot without a tenant", ErrEmptyID)
	}
	origin := HubOrigin(snap.TenantID)
	p := SnapshotPayloadV2{
		Type: PayloadSnapshot, TenantID: snap.TenantID, Version: snap.Version, SignedAt: snap.SignedAt.UTC(),
		Contacts: make([]wireContact, 0, len(snap.Contacts)),
		Groups:   make([]wireGroup, 0, len(snap.Groups)),
		Policies: make([]wirePolicy, 0, len(snap.Policies)),
	}
	for _, c := range snap.Contacts {
		wc := wireContact{Contact: c, Origin: origin, Addresses: make([]wireAddress, 0, len(c.Addresses))}
		for _, a := range c.Addresses {
			wc.Addresses = append(wc.Addresses, wireAddress{Address: a, Origin: origin})
		}
		p.Contacts = append(p.Contacts, wc)
	}
	for _, g := range snap.Groups {
		if g.MemberIDs == nil {
			g.MemberIDs = []string{}
		}
		p.Groups = append(p.Groups, wireGroup{Group: g, Origin: origin})
	}
	for _, pol := range snap.Policies {
		p.Policies = append(p.Policies, wirePolicy{DispatchPolicy: pol, Seeded: pol.Seeded, Origin: origin})
	}
	return json.Marshal(p)
}

// SealSnapshot builds and signs the v2 envelope of snap.
func (t *TrustAnchor) SealSnapshot(snap *Snapshot) (*Envelope, error) {
	payload, err := SnapshotPayload(snap)
	if err != nil {
		return nil, err
	}
	return t.Seal(payload)
}
