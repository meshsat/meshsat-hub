// Package oob is the Hub side of MeshSat OOB management frames
// (meshsat repo docs/OOB_MANAGEMENT_PROTOCOL.md v1.1, MESHSAT-756): short
// authenticated, optionally encrypted commands that reach a bridge over any
// bearer (SMS, Iridium MT via Cloudloop or Rock7) and come back the same way.
// The codec here is byte-compatible with the bridge's internal/oob and is
// pinned to the spec's test vectors in frame_test.go (MESHSAT-964 C).
package oob

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"io"
	"strings"
)

// Wire constants. See spec section 3.
const (
	Magic       byte = 0x4F // "O"
	Version     byte = 1
	FlagEnc     byte = 0x01
	FlagReply   byte = 0x02
	FlagNoReply byte = 0x04

	HeaderLen   = 9
	TagLen      = 16
	MinFrameLen = HeaderLen + TagLen
	MaxArgs     = 73
	MaxFrameLen = HeaderLen + MaxArgs + TagLen
	KeyLen      = 32
	NonceLen    = 12

	// Sentinel precedes the base32 text form on every bearer.
	Sentinel = "MS:"

	MinTextLen = 40
	MaxTextLen = 157

	peerIDLabel = "meshsat-oob-peer-id"
)

// Direction distinguishes a request from a reply in the nonce.
type Direction byte

// Directions.
const (
	DirRequest Direction = 0
	DirReply   Direction = 1
)

// Role is the side of a key relationship: the issuer of the key or the
// importer. Both sides use the same key; the role bit in the nonce keeps
// their counters apart.
type Role byte

// Roles.
const (
	RoleIssuer   Role = 0
	RoleImporter Role = 1
)

// Other returns the remote side's role.
func (r Role) Other() Role {
	if r == RoleIssuer {
		return RoleImporter
	}
	return RoleIssuer
}

// Frame is a decoded management frame.
type Frame struct {
	Enc     bool
	Reply   bool
	NoReply bool
	PeerID  uint16
	Counter uint32
	Cmd     byte
	Args    []byte
}

// Header is the pre-key view used by the classifier.
type Header struct {
	Flags   byte
	PeerID  uint16
	Counter uint32
	Cmd     byte
	BodyLen int
}

// Enc reports whether the body is encrypted.
func (h Header) Enc() bool { return h.Flags&FlagEnc != 0 }

// Reply reports whether the frame is a reply.
func (h Header) Reply() bool { return h.Flags&FlagReply != 0 }

// NoReply reports whether the sender asked for no answer.
func (h Header) NoReply() bool { return h.Flags&FlagNoReply != 0 }

// VersionNibble returns the version nibble.
func (h Header) VersionNibble() byte { return h.Flags >> 4 }

// Codec errors; every one means "not a frame for us".
var (
	ErrTooShort   = errors.New("oob: frame too short")
	ErrTooLong    = errors.New("oob: frame too long")
	ErrBadMagic   = errors.New("oob: bad magic")
	ErrBadVersion = errors.New("oob: unsupported version")
	ErrBadKey     = errors.New("oob: key must be 32 bytes")
	ErrBadPeer    = errors.New("oob: peer id must not be zero")
	ErrBadCounter = errors.New("oob: counter must not be zero")
	ErrArgsLen    = errors.New("oob: args exceed 73 bytes")
	ErrAuth       = errors.New("oob: authentication failed")
	ErrBadText    = errors.New("oob: invalid base32 text")
)

// ParseHeader validates magic, version and length bounds without cryptography.
func ParseHeader(wire []byte) (Header, error) {
	if len(wire) < MinFrameLen {
		return Header{}, ErrTooShort
	}
	if len(wire) > MaxFrameLen {
		return Header{}, ErrTooLong
	}
	if wire[0] != Magic {
		return Header{}, ErrBadMagic
	}
	h := Header{
		Flags:   wire[1],
		PeerID:  binary.BigEndian.Uint16(wire[2:4]),
		Counter: binary.BigEndian.Uint32(wire[4:8]),
		Cmd:     wire[8],
		BodyLen: len(wire) - HeaderLen - TagLen,
	}
	if h.VersionNibble() != Version {
		return Header{}, ErrBadVersion
	}
	return h, nil
}

// Nonce builds the deterministic 12-byte GCM nonce (never transmitted).
func Nonce(peerID uint16, senderRole Role, dir Direction, counter uint32) [NonceLen]byte {
	var n [NonceLen]byte
	binary.BigEndian.PutUint16(n[0:2], peerID)
	n[2] = byte(senderRole)<<1 | byte(dir)
	binary.BigEndian.PutUint32(n[3:7], counter)
	return n
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeyLen {
		return nil, ErrBadKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal builds the wire form of f under key; senderRole is this side's role.
func Seal(f Frame, key []byte, senderRole Role) ([]byte, error) {
	if f.PeerID == 0 {
		return nil, ErrBadPeer
	}
	if f.Counter == 0 {
		return nil, ErrBadCounter
	}
	if len(f.Args) > MaxArgs {
		return nil, ErrArgsLen
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, HeaderLen)
	hdr[0] = Magic
	flags := Version << 4
	if f.Enc {
		flags |= FlagEnc
	}
	dir := DirRequest
	if f.Reply {
		flags |= FlagReply
		dir = DirReply
	}
	if f.NoReply {
		flags |= FlagNoReply
	}
	hdr[1] = flags
	binary.BigEndian.PutUint16(hdr[2:4], f.PeerID)
	binary.BigEndian.PutUint32(hdr[4:8], f.Counter)
	hdr[8] = f.Cmd

	nonce := Nonce(f.PeerID, senderRole, dir, f.Counter)
	if f.Enc {
		return gcm.Seal(hdr, nonce[:], f.Args, hdr), nil
	}
	aad := append(append([]byte{}, hdr...), f.Args...)
	tag := gcm.Seal(nil, nonce[:], nil, aad)
	out := append(append([]byte{}, hdr...), f.Args...)
	return append(out, tag...), nil
}

// Open verifies and decodes wire under key; senderRole is the REMOTE side's
// role (the one that produced the frame).
func Open(wire []byte, key []byte, senderRole Role) (Frame, error) {
	h, err := ParseHeader(wire)
	if err != nil {
		return Frame{}, err
	}
	if h.PeerID == 0 {
		return Frame{}, ErrBadPeer
	}
	if h.Counter == 0 {
		return Frame{}, ErrBadCounter
	}
	gcm, err := newGCM(key)
	if err != nil {
		return Frame{}, err
	}
	dir := DirRequest
	if h.Reply() {
		dir = DirReply
	}
	nonce := Nonce(h.PeerID, senderRole, dir, h.Counter)
	hdr := wire[:HeaderLen]
	f := Frame{Enc: h.Enc(), Reply: h.Reply(), NoReply: h.NoReply(), PeerID: h.PeerID, Counter: h.Counter, Cmd: h.Cmd}
	if h.Enc() {
		args, err := gcm.Open(nil, nonce[:], wire[HeaderLen:], hdr)
		if err != nil {
			return Frame{}, ErrAuth
		}
		f.Args = args
		return f, nil
	}
	body := wire[HeaderLen : len(wire)-TagLen]
	tag := wire[len(wire)-TagLen:]
	aad := append(append([]byte{}, hdr...), body...)
	if _, err := gcm.Open(nil, nonce[:], tag, aad); err != nil {
		return Frame{}, ErrAuth
	}
	f.Args = append([]byte{}, body...)
	return f, nil
}

var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// Encode returns the text form: sentinel plus Crockford base32.
func Encode(wire []byte) string {
	return Sentinel + crockford.EncodeToString(wire)
}

func normalize(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		switch r {
		case 'I', 'L':
			r = '1'
		case 'O':
			r = '0'
		case '-':
			continue
		case 'U':
			return "", ErrBadText
		}
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
			continue
		}
		return "", ErrBadText
	}
	return b.String(), nil
}

// Decode parses the text form; a leading sentinel (any case) is optional.
func Decode(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if len(text) >= len(Sentinel) && strings.EqualFold(text[:len(Sentinel)], Sentinel) {
		text = text[len(Sentinel):]
	}
	norm, err := normalize(text)
	if err != nil {
		return nil, err
	}
	if len(norm) < MinTextLen || len(norm) > MaxTextLen {
		return nil, ErrBadText
	}
	wire, err := crockford.DecodeString(norm)
	if err != nil {
		return nil, ErrBadText
	}
	return wire, nil
}

func isRunChar(r byte) bool {
	return (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '-'
}

func boundaryBefore(text string, i int) bool {
	if i == 0 {
		return true
	}
	switch text[i-1] {
	case ' ', '\t', '\n', '\r', ']', '>', ':':
		return true
	}
	return false
}

// ExtractFrame scans text for a management frame and returns its wire form,
// never a partial result: the sentinel sits at a token boundary, the base32
// run is within bounds and the header parses.
func ExtractFrame(text string) ([]byte, bool) {
	from := 0
	for {
		idx := indexFold(text, Sentinel, from)
		if idx < 0 {
			return nil, false
		}
		from = idx + 1
		if !boundaryBefore(text, idx) {
			continue
		}
		start := idx + len(Sentinel)
		end := start
		for end < len(text) && isRunChar(text[end]) {
			end++
		}
		run := text[start:end]
		if len(run) < MinTextLen || len(run) > MaxTextLen+len(run)/2 {
			continue
		}
		wire, err := Decode(run)
		if err != nil {
			continue
		}
		if _, err := ParseHeader(wire); err != nil {
			continue
		}
		return wire, true
	}
}

func indexFold(s, sub string, from int) int {
	if from >= len(s) {
		return -1
	}
	upper := strings.ToUpper(s[from:])
	i := strings.Index(upper, strings.ToUpper(sub))
	if i < 0 {
		return -1
	}
	return from + i
}

// RandomKey returns a fresh 32-byte management key.
func RandomKey() ([]byte, error) {
	key := make([]byte, KeyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	return key, nil
}

// PeerIDFromKey derives the 16-bit wire peer id from the key (zero maps to 1).
func PeerIDFromKey(key []byte) uint16 {
	h := sha256.New()
	h.Write([]byte(peerIDLabel))
	h.Write(key)
	sum := h.Sum(nil)
	id := binary.BigEndian.Uint16(sum[:2])
	if id == 0 {
		id = 1
	}
	return id
}
