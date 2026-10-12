// Package federation lets one Hub hand a satellite payload to a device on
// another Hub, self-hosted or SaaS, after the two tenants have exchanged and
// pinned each other's certificates [MESHSAT-1549].
//
// The unit of trust is a tenant's federation identity: an Ed25519 key with a
// self-signed certificate. A self-hosted Hub is one tenant, so the same
// mechanism covers SaaS to SaaS, SaaS to self-hosted and self-hosted to
// self-hosted. Nothing is trusted through a CA: each owner adds the other's
// certificate by hand and compares its fingerprint out of band, so a peering
// exists only when BOTH sides have added each other (a sender only sends to a
// peer it added, a receiver only accepts a peer it added).
//
// A request is signed at the application layer rather than with client TLS,
// because the Hub sits behind an ingress that terminates TLS: the signature
// covers the method, the path, both fingerprints, the date and a digest of
// the body, and the receiver checks it against the PINNED certificate of the
// peer named in the request. mTLS end to end is a later phase.
package federation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/tlspin"
)

// Request headers. From and To are SPKI fingerprints (tlspin.SPKIHash).
const (
	HeaderFrom      = "X-MeshSat-Federation-From"
	HeaderTo        = "X-MeshSat-Federation-To"
	HeaderDate      = "X-MeshSat-Federation-Date"
	HeaderSignature = "X-MeshSat-Federation-Signature"
)

// RelayPath is the receiving endpoint on every Hub.
const RelayPath = "/api/federation/v1/relay"

// MaxClockSkew bounds how far a request's date may be from the receiver's
// clock. Replays inside the window are stopped by the message-id claim.
const MaxClockSkew = 5 * time.Minute

// MaxAttempts bounds the sender's retries of one message.
const MaxAttempts = 3

// MaxWireBytes bounds the relayed payload: an IMT message is at most 100 KB,
// and anything a 9704 can carry fits below this.
const MaxWireBytes = 100 * 1024

var (
	ErrNotEd25519   = errors.New("federation: certificate key is not Ed25519")
	ErrBadSignature = errors.New("federation: signature does not verify")
	ErrStale        = errors.New("federation: request date outside the allowed window")
	ErrMissing      = errors.New("federation: signature headers missing")
)

// Identity is one tenant's federation key and the certificate a peer pins.
type Identity struct {
	Key         ed25519.PrivateKey
	CertPEM     []byte
	Fingerprint string
}

// NewIdentity creates a key and a self-signed certificate. The label goes in
// the subject so an owner comparing certificates sees whose it is.
func NewIdentity(label string, now time.Time) (*Identity, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "MeshSat Hub federation: " + label},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Identity{
		Key:         key,
		CertPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Fingerprint: tlspin.SPKIHash(cert),
	}, nil
}

// KeyPEM renders the private key for sealed storage.
func (id *Identity) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(id.Key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// LoadIdentity reads a stored key and certificate and checks they pair up.
func LoadIdentity(keyPEM, certPEM []byte) (*Identity, error) {
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, errors.New("federation: key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, ErrNotEd25519
	}
	fp, pub, err := ParsePeerCert(certPEM)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(pub, key.Public().(ed25519.PublicKey)) {
		return nil, errors.New("federation: key and certificate do not pair up")
	}
	return &Identity{Key: key, CertPEM: certPEM, Fingerprint: fp}, nil
}

// ParsePeerCert reads a peer's certificate as pasted by an owner and returns
// its fingerprint and Ed25519 public key. Only the key matters: the
// certificate is a container for it, never checked against a CA or a date,
// because trust comes from the owner adding it, not from an issuer.
func ParsePeerCert(certPEM []byte) (string, ed25519.PublicKey, error) {
	b, _ := pem.Decode(bytes.TrimSpace(certPEM))
	if b == nil || b.Type != "CERTIFICATE" {
		return "", nil, errors.New("federation: not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return "", nil, err
	}
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", nil, ErrNotEd25519
	}
	return tlspin.SPKIHash(cert), pub, nil
}

func signingString(method, path, from, to, date string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{"meshsat-federation-v1", method, path, from, to, date, hex.EncodeToString(sum[:])}, "\n"))
}

// Sign sets the federation headers on an outgoing request carrying body.
func Sign(r *http.Request, body []byte, id *Identity, toFingerprint string, now time.Time) {
	date := now.UTC().Format(time.RFC3339)
	msg := signingString(r.Method, r.URL.Path, id.Fingerprint, toFingerprint, date, body)
	r.Header.Set(HeaderFrom, id.Fingerprint)
	r.Header.Set(HeaderTo, toFingerprint)
	r.Header.Set(HeaderDate, date)
	r.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(ed25519.Sign(id.Key, msg)))
}

// Verify checks an incoming request against the pinned key of the peer its
// From header names. The caller has already looked that peer up by the From
// fingerprint and the receiving tenant by the To fingerprint.
func Verify(r *http.Request, body []byte, peer ed25519.PublicKey, now time.Time) error {
	from, to := r.Header.Get(HeaderFrom), r.Header.Get(HeaderTo)
	date, sig := r.Header.Get(HeaderDate), r.Header.Get(HeaderSignature)
	if from == "" || to == "" || date == "" || sig == "" {
		return ErrMissing
	}
	t, err := time.Parse(time.RFC3339, date)
	if err != nil {
		return ErrStale
	}
	if d := now.Sub(t); d > MaxClockSkew || d < -MaxClockSkew {
		return ErrStale
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return ErrBadSignature
	}
	if !ed25519.Verify(peer, signingString(r.Method, r.URL.Path, from, to, date, body), raw) {
		return ErrBadSignature
	}
	return nil
}

// RelayFrame is the body of a relay request. The payload is never decoded or
// re-encoded on the way: the receiving device authenticates the bytes with
// keys no Hub holds.
type RelayFrame struct {
	// MsgID identifies the message on the sender's side; the receiver claims
	// it once, so a retry or a replay inside the date window sends nothing.
	MsgID string `json:"msg_id"`
	// Attempt counts the sender's tries for this message, from 1. The
	// receiver claims msg_id per attempt: a replay of one attempt sends
	// nothing, while a retry after a failed send is sent. A retry after a
	// send whose answer was lost can deliver a second copy; the receiving
	// Bridge drops a repeated MT and Reticulum a repeated packet, so that
	// costs one message where the opposite choice would lose one.
	Attempt  int    `json:"attempt"`
	FromIMEI string `json:"from_imei"`
	ToIMEI   string `json:"to_imei"`
	IMTTopic string `json:"imt_topic,omitempty"`
	WireB64  string `json:"wire_b64"`
}

var (
	imeiRE  = regexp.MustCompile(`^[0-9]{15}$`)
	msgIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	topicRE = regexp.MustCompile(`^[A-Z0-9_]{0,64}$`)
)

// Validate checks every field before anything is looked up.
func (f *RelayFrame) Validate() error {
	switch {
	case !msgIDRE.MatchString(f.MsgID):
		return errors.New("msg_id must be 1-128 characters of [A-Za-z0-9._:-]")
	case f.Attempt < 1 || f.Attempt > MaxAttempts:
		return fmt.Errorf("attempt must be 1 to %d", MaxAttempts)
	case !imeiRE.MatchString(f.FromIMEI) || !imeiRE.MatchString(f.ToIMEI):
		return errors.New("from_imei and to_imei must be 15 digits")
	case !topicRE.MatchString(f.IMTTopic):
		return errors.New("imt_topic is not an IMT topic name")
	}
	wire, err := base64.StdEncoding.DecodeString(f.WireB64)
	if err != nil || len(wire) == 0 {
		return errors.New("wire_b64 is not a base64 payload")
	}
	if len(wire) > MaxWireBytes {
		return fmt.Errorf("wire_b64 is %d bytes, over %d", len(wire), MaxWireBytes)
	}
	return nil
}
