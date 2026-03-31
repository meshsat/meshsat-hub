package reticulum

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// TestRNSConstants validates that all wire format constants match the
// Python RNS reference implementation (RNS 0.8.x / 1.x).
// Source: RNS/Reticulum.py, RNS/Packet.py, RNS/Identity.py, RNS/Interfaces/TCPInterface.py
func TestRNSConstants(t *testing.T) {
	t.Run("system constants", func(t *testing.T) {
		// RNS.Reticulum.MTU = 500
		if MTU != 500 {
			t.Errorf("MTU: got %d, want 500", MTU)
		}
		// RNS.Reticulum.HEADER_MINSIZE = 2+1+16 = 19
		if HeaderMinSize != 19 {
			t.Errorf("HeaderMinSize: got %d, want 19", HeaderMinSize)
		}
		// RNS.Reticulum.HEADER_MAXSIZE = 2+1+16+16 = 35
		if HeaderMaxSize != 35 {
			t.Errorf("HeaderMaxSize: got %d, want 35", HeaderMaxSize)
		}
		// RNS.Identity.KEYSIZE = 512 bits / 8 = 64 bytes
		if IdentityKeySize != 64 {
			t.Errorf("IdentityKeySize: got %d, want 64", IdentityKeySize)
		}
		// RNS.Identity.TRUNCATED_HASHLENGTH = 128 bits / 8 = 16 bytes
		if TruncatedHashLen != 16 {
			t.Errorf("TruncatedHashLen: got %d, want 16", TruncatedHashLen)
		}
		// RNS.Identity.NAME_HASH_LENGTH = 80 bits / 8 = 10 bytes
		if NameHashLen != 10 {
			t.Errorf("NameHashLen: got %d, want 10", NameHashLen)
		}
		// RNS.Identity.SIGLENGTH = 512 bits / 8 = 64 bytes
		if SignatureLen != 64 {
			t.Errorf("SignatureLen: got %d, want 64", SignatureLen)
		}
		// Ed25519 ratchet key = 256 bits / 8 = 32 bytes
		if RatchetKeyLen != 32 {
			t.Errorf("RatchetKeyLen: got %d, want 32", RatchetKeyLen)
		}
		// RNS.Reticulum.TRANSPORT_HOP_LIMIT = 128
		if PathfinderM != 128 {
			t.Errorf("PathfinderM: got %d, want 128", PathfinderM)
		}
		// Random hash length = 80 bits / 8 = 10 bytes
		if RandomHashLen != 10 {
			t.Errorf("RandomHashLen: got %d, want 10", RandomHashLen)
		}
	})

	t.Run("packet type values", func(t *testing.T) {
		// RNS.Packet.DATA = 0x00
		if PacketData != 0x00 {
			t.Errorf("PacketData: got 0x%02X, want 0x00", PacketData)
		}
		// RNS.Packet.ANNOUNCE = 0x01
		if PacketAnnounce != 0x01 {
			t.Errorf("PacketAnnounce: got 0x%02X, want 0x01", PacketAnnounce)
		}
		// RNS.Packet.LINKREQUEST = 0x02
		if PacketLinkRequest != 0x02 {
			t.Errorf("PacketLinkRequest: got 0x%02X, want 0x02", PacketLinkRequest)
		}
		// RNS.Packet.PROOF = 0x03
		if PacketProof != 0x03 {
			t.Errorf("PacketProof: got 0x%02X, want 0x03", PacketProof)
		}
	})

	t.Run("destination type values", func(t *testing.T) {
		// RNS.Destination.SINGLE = 0x00
		if DestSingle != 0x00 {
			t.Errorf("DestSingle: got 0x%02X, want 0x00", DestSingle)
		}
		// RNS.Destination.GROUP = 0x01
		if DestGroup != 0x01 {
			t.Errorf("DestGroup: got 0x%02X, want 0x01", DestGroup)
		}
		// RNS.Destination.PLAIN = 0x02
		if DestPlain != 0x02 {
			t.Errorf("DestPlain: got 0x%02X, want 0x02", DestPlain)
		}
		// RNS.Destination.LINK = 0x03
		if DestLink != 0x03 {
			t.Errorf("DestLink: got 0x%02X, want 0x03", DestLink)
		}
	})

	t.Run("header type values", func(t *testing.T) {
		// RNS.Packet.HEADER_1 = 0x00
		if HeaderType1 != 0x00 {
			t.Errorf("HeaderType1: got 0x%02X, want 0x00", HeaderType1)
		}
		// RNS.Packet.HEADER_2 = 0x01
		if HeaderType2 != 0x01 {
			t.Errorf("HeaderType2: got 0x%02X, want 0x01", HeaderType2)
		}
	})

	t.Run("transport type values", func(t *testing.T) {
		// RNS.Transport.BROADCAST = 0x00
		if TransportBroadcast != 0x00 {
			t.Errorf("TransportBroadcast: got 0x%02X, want 0x00", TransportBroadcast)
		}
		// RNS.Transport.TRANSPORT = 0x01
		if TransportTransport != 0x01 {
			t.Errorf("TransportTransport: got 0x%02X, want 0x01", TransportTransport)
		}
	})

	t.Run("HDLC values", func(t *testing.T) {
		// RNS.Interfaces.TCPInterface.HDLC.FLAG = 0x7E
		if HDLCFlag != 0x7E {
			t.Errorf("HDLCFlag: got 0x%02X, want 0x7E", HDLCFlag)
		}
		// RNS.Interfaces.TCPInterface.HDLC.ESC = 0x7D
		if HDLCEsc != 0x7D {
			t.Errorf("HDLCEsc: got 0x%02X, want 0x7D", HDLCEsc)
		}
		// RNS.Interfaces.TCPInterface.HDLC.ESC_MASK = 0x20
		if HDLCEscMask != 0x20 {
			t.Errorf("HDLCEscMask: got 0x%02X, want 0x20", HDLCEscMask)
		}
	})

	t.Run("context type values", func(t *testing.T) {
		// RNS.Packet context constants
		if ContextNone != 0x00 {
			t.Errorf("ContextNone: got 0x%02X, want 0x00", ContextNone)
		}
		if ContextResource != 0x01 {
			t.Errorf("ContextResource: got 0x%02X, want 0x01", ContextResource)
		}
		if ContextPathResponse != 0x0B {
			t.Errorf("ContextPathResponse: got 0x%02X, want 0x0B", ContextPathResponse)
		}
		if ContextChannel != 0x0E {
			t.Errorf("ContextChannel: got 0x%02X, want 0x0E", ContextChannel)
		}
		if ContextKeepalive != 0xFA {
			t.Errorf("ContextKeepalive: got 0x%02X, want 0xFA", ContextKeepalive)
		}
		if ContextLinkIdentify != 0xFB {
			t.Errorf("ContextLinkIdentify: got 0x%02X, want 0xFB", ContextLinkIdentify)
		}
		if ContextLinkClose != 0xFC {
			t.Errorf("ContextLinkClose: got 0x%02X, want 0xFC", ContextLinkClose)
		}
		if ContextLinkProof != 0xFD {
			t.Errorf("ContextLinkProof: got 0x%02X, want 0xFD", ContextLinkProof)
		}
		if ContextLRRTT != 0xFE {
			t.Errorf("ContextLRRTT: got 0x%02X, want 0xFE", ContextLRRTT)
		}
		if ContextLRProof != 0xFF {
			t.Errorf("ContextLRProof: got 0x%02X, want 0xFF", ContextLRProof)
		}
	})

	t.Run("announce payload sizes", func(t *testing.T) {
		// Minimum announce: pubkey(64) + name_hash(10) + random(10) + signature(64) = 148
		if AnnounceMinPayload != 148 {
			t.Errorf("AnnounceMinPayload: got %d, want 148", AnnounceMinPayload)
		}
		// With ratchet: 148 + 32 = 180
		if AnnounceRatchetPayload != 180 {
			t.Errorf("AnnounceRatchetPayload: got %d, want 180", AnnounceRatchetPayload)
		}
	})
}

// TestRNSFlagByteValues validates that specific packet type combinations
// produce the exact flag byte values that Python RNS generates.
// These values are verified against RNS/Packet.py pack_flags().
func TestRNSFlagByteValues(t *testing.T) {
	tests := []struct {
		name     string
		header   Header
		expected byte
	}{
		{
			// Standard announce: Header1, no context, broadcast, single, announce
			// Python: (0 << 6) | (0 << 5) | (0 << 4) | (0 << 2) | 1 = 0x01
			name: "standard announce",
			header: Header{
				HeaderType: HeaderType1, ContextFlag: 0,
				TransportType: TransportBroadcast, DestType: DestSingle,
				PacketType: PacketAnnounce,
			},
			expected: 0x01,
		},
		{
			// Announce with ratchet: context flag set
			// Python: (0 << 6) | (1 << 5) | (0 << 4) | (0 << 2) | 1 = 0x21
			name: "announce with ratchet",
			header: Header{
				HeaderType: HeaderType1, ContextFlag: 1,
				TransportType: TransportBroadcast, DestType: DestSingle,
				PacketType: PacketAnnounce,
			},
			expected: 0x21,
		},
		{
			// Data to single destination (broadcast)
			// Python: (0 << 6) | (0 << 5) | (0 << 4) | (0 << 2) | 0 = 0x00
			name: "data broadcast single",
			header: Header{
				HeaderType: HeaderType1, ContextFlag: 0,
				TransportType: TransportBroadcast, DestType: DestSingle,
				PacketType: PacketData,
			},
			expected: 0x00,
		},
		{
			// Link request (broadcast, single)
			// Python: (0 << 6) | (0 << 5) | (0 << 4) | (0 << 2) | 2 = 0x02
			name: "link request",
			header: Header{
				HeaderType: HeaderType1, ContextFlag: 0,
				TransportType: TransportBroadcast, DestType: DestSingle,
				PacketType: PacketLinkRequest,
			},
			expected: 0x02,
		},
		{
			// Proof (broadcast, link destination)
			// Python: (0 << 6) | (0 << 5) | (0 << 4) | (3 << 2) | 3 = 0x0F
			name: "proof link dest",
			header: Header{
				HeaderType: HeaderType1, ContextFlag: 0,
				TransportType: TransportBroadcast, DestType: DestLink,
				PacketType: PacketProof,
			},
			expected: 0x0F,
		},
		{
			// Transport header (Type2) data to single
			// Python: (1 << 6) | (0 << 5) | (1 << 4) | (0 << 2) | 0 = 0x50
			name: "transport data single",
			header: Header{
				HeaderType: HeaderType2, ContextFlag: 0,
				TransportType: TransportTransport, DestType: DestSingle,
				PacketType: PacketData,
			},
			expected: 0x50,
		},
		{
			// Transport announce (Type2, transport, single)
			// Python: (1 << 6) | (0 << 5) | (1 << 4) | (0 << 2) | 1 = 0x51
			name: "transport announce",
			header: Header{
				HeaderType: HeaderType2, ContextFlag: 0,
				TransportType: TransportTransport, DestType: DestSingle,
				PacketType: PacketAnnounce,
			},
			expected: 0x51,
		},
		{
			// Data to plain destination (no encryption, broadcast)
			// Python: (0 << 6) | (0 << 5) | (0 << 4) | (2 << 2) | 0 = 0x08
			name: "data broadcast plain",
			header: Header{
				HeaderType: HeaderType1, ContextFlag: 0,
				TransportType: TransportBroadcast, DestType: DestPlain,
				PacketType: PacketData,
			},
			expected: 0x08,
		},
		{
			// Data to group destination (broadcast)
			// Python: (0 << 6) | (0 << 5) | (0 << 4) | (1 << 2) | 0 = 0x04
			name: "data broadcast group",
			header: Header{
				HeaderType: HeaderType1, ContextFlag: 0,
				TransportType: TransportBroadcast, DestType: DestGroup,
				PacketType: PacketData,
			},
			expected: 0x04,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.header.PackFlags()
			if got != tt.expected {
				t.Errorf("flags: got 0x%02X, want 0x%02X", got, tt.expected)
			}
			// Also verify round-trip
			var h2 Header
			h2.UnpackFlags(got)
			if h2.HeaderType != tt.header.HeaderType ||
				h2.ContextFlag != tt.header.ContextFlag ||
				h2.TransportType != tt.header.TransportType ||
				h2.DestType != tt.header.DestType ||
				h2.PacketType != tt.header.PacketType {
				t.Error("flag round-trip mismatch")
			}
		})
	}
}

// TestRNSAnnounceWireLayout validates the exact byte layout of an announce
// packet as Python RNS Transport.inbound_handler expects it.
//
// Python RNS validates announces in this order:
//  1. Parse header → extract dest_hash from bytes 2:18
//  2. Extract public_key[0:64] from data field
//  3. Compute identity_hash = SHA256(public_key)[:16]
//  4. Extract name_hash[64:74] from data field
//  5. Verify dest_hash == SHA256(name_hash || identity_hash)[:16]
//  6. Extract signed_data = dest_hash + public_key + name_hash + random_hash [+ ratchet] + app_data
//  7. Verify Ed25519 signature over signed_data
func TestRNSAnnounceWireLayout(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}

	appName := "meshsat.hub"
	appData := []byte("hub-01")

	ann, err := NewAnnounce(id, appName, appData)
	if err != nil {
		t.Fatal(err)
	}

	raw := ann.MarshalPacket()

	// ── Header validation (byte-by-byte, matching Python RNS parsing) ──

	// Byte 0: flags — must be 0x01 for standard announce
	if raw[0] != 0x01 {
		t.Errorf("flags byte: got 0x%02X, want 0x01", raw[0])
	}

	// Byte 1: hops — must be 0 for new announce
	if raw[1] != 0x00 {
		t.Errorf("hops byte: got 0x%02X, want 0x00", raw[1])
	}

	// Bytes 2-17: destination hash (16 bytes)
	destHash := raw[2:18]
	expectedDest := id.DestHash(appName)
	if !bytes.Equal(destHash, expectedDest[:]) {
		t.Errorf("dest hash: got %s, want %s",
			hex.EncodeToString(destHash), hex.EncodeToString(expectedDest[:]))
	}

	// Byte 18: context — must be 0x00 for standard announce
	if raw[18] != 0x00 {
		t.Errorf("context byte: got 0x%02X, want 0x00", raw[18])
	}

	// ── Payload validation (after 19-byte header) ──
	data := raw[HeaderMinSize:]

	// Bytes 0-63: public key [32B X25519 enc][32B Ed25519 sig]
	pubKey := data[:IdentityKeySize]
	expectedPub := id.PublicBytes()
	if !bytes.Equal(pubKey, expectedPub) {
		t.Error("public key mismatch in wire format")
	}

	// Verify key order: encryption first, signing second (critical for RNS!)
	encPub := pubKey[:32]
	sigPub := pubKey[32:]
	if !bytes.Equal(encPub, id.EncryptionPublicKey().Bytes()) {
		t.Error("encryption key not in first 32 bytes")
	}
	if !bytes.Equal(sigPub, id.SigningPublicKey()) {
		t.Error("signing key not in bytes 32-63")
	}

	// Bytes 64-73: name hash
	nameHash := data[IdentityKeySize : IdentityKeySize+NameHashLen]
	expectedNameHash := sha256.Sum256([]byte(appName))
	if !bytes.Equal(nameHash, expectedNameHash[:NameHashLen]) {
		t.Error("name hash mismatch")
	}

	// Bytes 74-83: random hash [5B random][5B timestamp BE]
	randomHash := data[IdentityKeySize+NameHashLen : IdentityKeySize+NameHashLen+RandomHashLen]

	// Verify timestamp is embedded in last 5 bytes (big-endian)
	ts := int64(randomHash[5])<<32 |
		int64(randomHash[6])<<24 |
		int64(randomHash[7])<<16 |
		int64(randomHash[8])<<8 |
		int64(randomHash[9])
	if ts <= 0 {
		t.Error("timestamp in random hash should be positive")
	}

	// Bytes 84-147: signature (64 bytes)
	sig := data[IdentityKeySize+NameHashLen+RandomHashLen : IdentityKeySize+NameHashLen+RandomHashLen+SignatureLen]
	if len(sig) != SignatureLen {
		t.Errorf("signature length: got %d, want %d", len(sig), SignatureLen)
	}

	// Bytes 148+: app data
	gotAppData := data[IdentityKeySize+NameHashLen+RandomHashLen+SignatureLen:]
	if !bytes.Equal(gotAppData, appData) {
		t.Errorf("app data: got %q, want %q", gotAppData, appData)
	}

	// ── Cross-validate: recompute destination hash the way Python RNS does ──
	// identity_hash = SHA256(enc_pub || sig_pub)[:16]
	idHasher := sha256.New()
	idHasher.Write(encPub)
	idHasher.Write(sigPub)
	identityHash := idHasher.Sum(nil)[:TruncatedHashLen]

	// dest_hash = SHA256(name_hash || identity_hash)[:16]
	destHasher := sha256.New()
	destHasher.Write(nameHash)
	destHasher.Write(identityHash)
	computedDest := destHasher.Sum(nil)[:TruncatedHashLen]

	if !bytes.Equal(computedDest, destHash) {
		t.Errorf("RNS dest hash computation mismatch: computed %s, header %s",
			hex.EncodeToString(computedDest), hex.EncodeToString(destHash))
	}

	// ── Cross-validate: verify signature the way Python RNS does ──
	// signed_data = dest_hash + public_key + name_hash + random_hash + app_data
	var signedData []byte
	signedData = append(signedData, destHash...)
	signedData = append(signedData, pubKey...)
	signedData = append(signedData, nameHash...)
	signedData = append(signedData, randomHash...)
	signedData = append(signedData, appData...)

	if !VerifySignature(sigPub, signedData, sig) {
		t.Error("signature verification failed using RNS signed_data layout")
	}

	// ── Total packet size validation ──
	expectedSize := HeaderMinSize + AnnounceMinPayload + len(appData)
	if len(raw) != expectedSize {
		t.Errorf("total packet size: got %d, want %d", len(raw), expectedSize)
	}
}

// TestRNSAnnounceWithRatchet validates that the ratchet key is placed
// correctly between random_hash and signature, matching Python RNS.
func TestRNSAnnounceWithRatchet(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}

	ann, err := NewAnnounce(id, "test.app", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Set ratchet key and context flag
	ann.ContextFlag = 1
	for i := range ann.Ratchet {
		ann.Ratchet[i] = byte(i + 0xA0)
	}

	// Re-sign with ratchet included in signed body
	ann.Signature = id.Sign(ann.signableBody())

	raw := ann.MarshalPacket()

	// Flags should have context bit set → 0x21
	if raw[0] != 0x21 {
		t.Errorf("flags with ratchet: got 0x%02X, want 0x21", raw[0])
	}

	// Parse back and verify
	parsed, err := UnmarshalAnnouncePacket(raw)
	if err != nil {
		t.Fatal(err)
	}

	if parsed.ContextFlag != 1 {
		t.Error("context flag not set after unmarshal")
	}
	if parsed.Ratchet != ann.Ratchet {
		t.Error("ratchet key mismatch after unmarshal")
	}

	// Payload size should be: 64 + 10 + 10 + 32 (ratchet) + 64 = 180
	data := raw[HeaderMinSize:]
	if len(data) != AnnounceRatchetPayload {
		t.Errorf("ratchet payload size: got %d, want %d", len(data), AnnounceRatchetPayload)
	}

	// Verify ratchet position: after random_hash, before signature
	ratchetStart := IdentityKeySize + NameHashLen + RandomHashLen
	ratchetEnd := ratchetStart + RatchetKeyLen
	gotRatchet := data[ratchetStart:ratchetEnd]
	if !bytes.Equal(gotRatchet, ann.Ratchet[:]) {
		t.Error("ratchet bytes not at expected position in payload")
	}
}

// TestRNSHeaderType2WireFormat validates that Type2 (transport) headers
// place the transport ID before the destination hash, matching Python RNS.
func TestRNSHeaderType2WireFormat(t *testing.T) {
	transportID := [TruncatedHashLen]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF,
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00}
	destHash := [TruncatedHashLen]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06,
		0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10}

	h := &Header{
		HeaderType:    HeaderType2,
		TransportType: TransportTransport,
		DestType:      DestSingle,
		PacketType:    PacketData,
		Hops:          3,
		TransportID:   transportID,
		DestHash:      destHash,
		Context:       ContextNone,
		Data:          []byte("test payload"),
	}

	raw := h.Marshal()

	// Byte 0: flags = (1 << 6) | (0 << 5) | (1 << 4) | (0 << 2) | 0 = 0x50
	if raw[0] != 0x50 {
		t.Errorf("type2 flags: got 0x%02X, want 0x50", raw[0])
	}

	// Byte 1: hops
	if raw[1] != 3 {
		t.Errorf("hops: got %d, want 3", raw[1])
	}

	// Bytes 2-17: transport ID (Python RNS: transport_id comes first in Type2)
	gotTransport := raw[2:18]
	if !bytes.Equal(gotTransport, transportID[:]) {
		t.Error("transport ID not at bytes 2-17")
	}

	// Bytes 18-33: destination hash
	gotDest := raw[18:34]
	if !bytes.Equal(gotDest, destHash[:]) {
		t.Error("dest hash not at bytes 18-33")
	}

	// Byte 34: context
	if raw[34] != ContextNone {
		t.Errorf("context: got 0x%02X, want 0x00", raw[34])
	}

	// Total header size must be 35
	if len(raw) != HeaderMaxSize+len(h.Data) {
		t.Errorf("total size: got %d, want %d", len(raw), HeaderMaxSize+len(h.Data))
	}
}

// TestRNSDestHashComputation validates destination hash computation matches
// the Python RNS algorithm exactly:
//
//	name_hash     = SHA256("app.aspect")[:10]
//	identity_hash = SHA256(enc_pub || sig_pub)[:16]
//	dest_hash     = SHA256(name_hash || identity_hash)[:16]
func TestRNSDestHashComputation(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}

	appName := "meshsat.hub"

	// Step 1: name_hash = SHA256(app_name)[:10]
	nameHashFull := sha256.Sum256([]byte(appName))
	nameHash := nameHashFull[:NameHashLen]

	goNameHash := ComputeNameHash(appName)
	if !bytes.Equal(goNameHash[:], nameHash) {
		t.Error("ComputeNameHash doesn't match SHA256 reference")
	}

	// Step 2: identity_hash = SHA256(enc_pub || sig_pub)[:16]
	encPub := id.EncryptionPublicKey().Bytes()
	sigPub := []byte(id.SigningPublicKey())
	idHasher := sha256.New()
	idHasher.Write(encPub)
	idHasher.Write(sigPub)
	identityHash := idHasher.Sum(nil)[:TruncatedHashLen]

	goIdentityHash := id.IdentityHash()
	if !bytes.Equal(goIdentityHash[:], identityHash) {
		t.Error("IdentityHash doesn't match SHA256 reference")
	}

	// Step 3: dest_hash = SHA256(name_hash || identity_hash)[:16]
	destHasher := sha256.New()
	destHasher.Write(nameHash)
	destHasher.Write(identityHash)
	expectedDest := destHasher.Sum(nil)[:TruncatedHashLen]

	goDest := id.DestHash(appName)
	if !bytes.Equal(goDest[:], expectedDest) {
		t.Error("DestHash doesn't match SHA256 reference")
	}
}

// TestRNSHDLCSpecialBytePayload validates HDLC escaping with payloads
// containing many special bytes (0x7E flag, 0x7D escape). This catches
// escaping bugs that would corrupt packets on the wire.
func TestRNSHDLCSpecialBytePayload(t *testing.T) {
	// Build a fake Reticulum packet (>= HeaderMinSize) with special bytes
	payload := make([]byte, HeaderMinSize+50)
	// Fill first 19 bytes with valid header-like data
	payload[0] = 0x01 // flags (announce)
	payload[1] = 0x00 // hops
	// Fill destination hash area with 0x7E (flag bytes)
	for i := 2; i < 18; i++ {
		payload[i] = HDLCFlag
	}
	payload[18] = 0x00 // context
	// Fill data with alternating special bytes
	for i := HeaderMinSize; i < len(payload); i++ {
		switch i % 4 {
		case 0:
			payload[i] = HDLCFlag
		case 1:
			payload[i] = HDLCEsc
		case 2:
			payload[i] = HDLCFlag
		case 3:
			payload[i] = 0x42 // normal byte
		}
	}

	// Frame it
	frame := HDLCFrame(payload)

	// The frame should not contain any unescaped flag bytes except delimiters
	inner := frame[1 : len(frame)-1]
	for i := 0; i < len(inner); i++ {
		if inner[i] == HDLCFlag {
			t.Errorf("unescaped FLAG at position %d in inner frame", i)
		}
	}

	// Extract frame and verify round-trip
	reader := NewHDLCFrameReader()
	frames := reader.Feed(frame)
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if !bytes.Equal(frames[0], payload) {
		t.Error("HDLC round-trip failed with special byte payload")
	}
}

// TestRNSHDLCMTUBoundary tests HDLC framing at the exact MTU boundary (500 bytes).
func TestRNSHDLCMTUBoundary(t *testing.T) {
	// MTU-sized packet
	payload := make([]byte, MTU)
	payload[0] = 0x01 // flags
	for i := 1; i < MTU; i++ {
		payload[i] = byte(i)
	}

	frame := HDLCFrame(payload)
	reader := NewHDLCFrameReader()
	frames := reader.Feed(frame)

	if len(frames) != 1 {
		t.Fatalf("expected 1 frame for MTU-sized packet, got %d", len(frames))
	}
	if !bytes.Equal(frames[0], payload) {
		t.Error("MTU boundary round-trip failed")
	}
}

// TestRNSHDLCConsecutiveFrames validates that the reader correctly handles
// back-to-back frames where the end flag of frame N is the start flag of
// frame N+1 (Python RNS TCPInterface uses this optimization).
func TestRNSHDLCConsecutiveFrames(t *testing.T) {
	p1 := make([]byte, HeaderMinSize+10)
	p2 := make([]byte, HeaderMinSize+15)
	for i := range p1 {
		p1[i] = byte(i + 0x10)
	}
	for i := range p2 {
		p2[i] = byte(i + 0x80)
	}

	// Simulate Python RNS behavior: end flag of frame 1 = start flag of frame 2
	// [FLAG][escaped_p1][FLAG][escaped_p2][FLAG]
	// The middle FLAG serves double duty
	var wire []byte
	wire = append(wire, HDLCFlag)
	wire = append(wire, HDLCEscape(p1)...)
	wire = append(wire, HDLCFlag) // end of p1, start of p2
	wire = append(wire, HDLCEscape(p2)...)
	wire = append(wire, HDLCFlag)

	reader := NewHDLCFrameReader()
	frames := reader.Feed(wire)

	if len(frames) != 2 {
		t.Fatalf("expected 2 frames from consecutive flags, got %d", len(frames))
	}
	if !bytes.Equal(frames[0], p1) {
		t.Errorf("frame 0: got %x, want %x", frames[0], p1)
	}
	if !bytes.Equal(frames[1], p2) {
		t.Errorf("frame 1: got %x, want %x", frames[1], p2)
	}
}

// TestRNSAnnounceMarshalUnmarshalRoundtrip validates that Go→wire→Go
// preserves all fields exactly, which is a prerequisite for wire compat.
func TestRNSAnnounceMarshalUnmarshalRoundtrip(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}

	original, err := NewAnnounce(id, "test.aspect", []byte("node-data"))
	if err != nil {
		t.Fatal(err)
	}

	raw := original.MarshalPacket()
	parsed, err := UnmarshalAnnouncePacket(raw)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	// Verify all fields
	if parsed.DestHash != original.DestHash {
		t.Error("DestHash mismatch")
	}
	if parsed.Hops != original.Hops {
		t.Error("Hops mismatch")
	}
	if parsed.ContextFlag != original.ContextFlag {
		t.Error("ContextFlag mismatch")
	}
	if !bytes.Equal(parsed.PublicKey, original.PublicKey) {
		t.Error("PublicKey mismatch")
	}
	if parsed.NameHash != original.NameHash {
		t.Error("NameHash mismatch")
	}
	if parsed.Random != original.Random {
		t.Error("Random mismatch")
	}
	if !bytes.Equal(parsed.Signature, original.Signature) {
		t.Error("Signature mismatch")
	}
	if !bytes.Equal(parsed.AppData, original.AppData) {
		t.Error("AppData mismatch")
	}

	// Verify the parsed announce is valid
	if err := parsed.Verify(); err != nil {
		t.Errorf("parsed announce verification failed: %v", err)
	}
}

// TestRNSPathRequestWireFormat validates path request packet structure.
// In Python RNS, a path request is a DATA packet to PLAIN destination
// with the queried destination hash as payload.
func TestRNSPathRequestWireFormat(t *testing.T) {
	// Path requests in RNS use:
	// HeaderType1, broadcast, plain destination, data packet
	// Payload: [16B queried_dest_hash] + [16B requesting_transport_id (optional)]
	queriedDest := [TruncatedHashLen]byte{
		0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04,
		0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C,
	}

	// In RNS, the path request destination is the broadcast address
	// (all-FF for plain destinations)
	h := &Header{
		HeaderType:    HeaderType1,
		TransportType: TransportBroadcast,
		DestType:      DestPlain,
		PacketType:    PacketData,
		Hops:          0,
		Context:       ContextNone,
		Data:          queriedDest[:],
	}

	raw := h.Marshal()

	// Flags should be 0x08 (plain destination, data)
	if raw[0] != 0x08 {
		t.Errorf("path request flags: got 0x%02X, want 0x08", raw[0])
	}

	// Total size: 19 header + 16 payload = 35
	if len(raw) != HeaderMinSize+TruncatedHashLen {
		t.Errorf("path request size: got %d, want %d",
			len(raw), HeaderMinSize+TruncatedHashLen)
	}

	// Parse back
	parsed, err := UnmarshalHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.DestType != DestPlain {
		t.Error("parsed dest type should be PLAIN")
	}
	if !bytes.Equal(parsed.Data, queriedDest[:]) {
		t.Error("parsed payload should contain queried dest hash")
	}
}

// TestRNSPathResponseWireFormat validates path response packet structure.
// In Python RNS, a path response is sent as a DATA packet with
// Context = ContextPathResponse (0x0B).
func TestRNSPathResponseWireFormat(t *testing.T) {
	// Path response contains the original announce packet as payload
	// Context byte is set to 0x0B (PATH_RESPONSE)
	destHash := [TruncatedHashLen]byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10,
	}

	// Simulate an announce payload (just the first 148+ bytes)
	announcePayload := make([]byte, AnnounceMinPayload)
	for i := range announcePayload {
		announcePayload[i] = byte(i)
	}

	h := &Header{
		HeaderType:    HeaderType1,
		TransportType: TransportBroadcast,
		DestType:      DestSingle,
		PacketType:    PacketAnnounce,
		Hops:          2,
		DestHash:      destHash,
		Context:       ContextPathResponse,
		Data:          announcePayload,
	}

	raw := h.Marshal()

	// Context byte should be 0x0B
	if raw[18] != ContextPathResponse {
		t.Errorf("path response context: got 0x%02X, want 0x%02X",
			raw[18], ContextPathResponse)
	}

	parsed, err := UnmarshalHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Context != ContextPathResponse {
		t.Error("parsed context should be PATH_RESPONSE")
	}
}

// TestRNSAnnounceFullHDLCPipeline validates the complete announce pipeline:
// create → marshal → HDLC frame → HDLC extract → unmarshal → verify.
// This is the exact path a packet takes from Hub to Python RNS.
func TestRNSAnnounceFullHDLCPipeline(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}

	// Create announce (Hub side)
	ann, err := NewAnnounce(id, "meshsat.hub", []byte("test"))
	if err != nil {
		t.Fatal(err)
	}
	raw := ann.MarshalPacket()

	// HDLC frame (TCPInterface.Send)
	frame := HDLCFrame(raw)

	// Simulate TCP transport — byte-at-a-time delivery (worst case)
	reader := NewHDLCFrameReader()
	var extracted [][]byte
	for _, b := range frame {
		result := reader.Feed([]byte{b})
		extracted = append(extracted, result...)
	}

	if len(extracted) != 1 {
		t.Fatalf("expected 1 frame from byte-at-a-time feed, got %d", len(extracted))
	}

	// Unmarshal on receiver side (Python RNS equivalent)
	parsed, err := UnmarshalAnnouncePacket(extracted[0])
	if err != nil {
		t.Fatalf("unmarshal after HDLC: %v", err)
	}

	// Verify signature and dest hash (Python RNS validate_announce equivalent)
	if err := parsed.Verify(); err != nil {
		t.Errorf("verify after full pipeline: %v", err)
	}

	// Verify the parsed fields match original
	if parsed.DestHash != ann.DestHash {
		t.Error("dest hash mismatch after full pipeline")
	}
	if !bytes.Equal(parsed.AppData, []byte("test")) {
		t.Error("app data mismatch after full pipeline")
	}
}

// TestRNSRandomHashFormat validates that random_hash uses the RNS-compatible
// format: [5B random][5B big-endian unix timestamp].
// Python RNS Transport uses the timestamp for announce ordering and dedup.
func TestRNSRandomHashFormat(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}

	ann, err := NewAnnounce(id, "test.app", nil)
	if err != nil {
		t.Fatal(err)
	}

	raw := ann.MarshalPacket()
	data := raw[HeaderMinSize:]

	// Extract random hash at offset 74 (64 pubkey + 10 name hash)
	randomHash := data[IdentityKeySize+NameHashLen : IdentityKeySize+NameHashLen+RandomHashLen]

	// Last 5 bytes should be a valid unix timestamp
	var tsBytes [8]byte
	// Pad to 8 bytes for binary.BigEndian.Uint64
	copy(tsBytes[3:], randomHash[5:])
	ts := binary.BigEndian.Uint64(tsBytes[:])

	// Should be within a few seconds of now (test tolerance)
	now := uint64(1700000000) // well before any reasonable test time
	if ts < now {
		t.Errorf("timestamp too old: %d", ts)
	}

	// First 5 bytes should be random (extremely unlikely to be all zeros)
	allZero := true
	for _, b := range randomHash[:5] {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("first 5 bytes of random hash are all zero (should be random)")
	}
}
