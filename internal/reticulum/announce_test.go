package reticulum

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"
	"time"
)

func TestNewAnnounce_Basic(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}

	a, err := NewAnnounce(id, "meshsat.hub", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(a.PublicKey) != IdentityKeySize {
		t.Errorf("PublicKey size: got %d, want %d", len(a.PublicKey), IdentityKeySize)
	}
	if len(a.Signature) != SignatureLen {
		t.Errorf("Signature size: got %d, want %d", len(a.Signature), SignatureLen)
	}
	if a.Hops != 0 {
		t.Errorf("Hops: got %d, want 0", a.Hops)
	}
	if a.Random == [RandomHashLen]byte{} {
		t.Error("Random should not be zero")
	}
}

func TestNewAnnounce_WithAppData(t *testing.T) {
	id, _ := GenerateIdentity()
	appData := []byte(`{"name":"hub-01","version":"1.0"}`)

	a, err := NewAnnounce(id, "meshsat.hub", appData)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(a.AppData, appData) {
		t.Error("AppData mismatch")
	}
}

func TestAnnounce_Verify(t *testing.T) {
	id, _ := GenerateIdentity()
	a, _ := NewAnnounce(id, "meshsat.hub", []byte("metadata"))

	if err := a.Verify(); err != nil {
		t.Fatalf("valid announce should verify: %v", err)
	}
}

func TestAnnounce_VerifyFailsOnTamperedSignature(t *testing.T) {
	id, _ := GenerateIdentity()
	a, _ := NewAnnounce(id, "meshsat.hub", nil)

	a.Signature[0] ^= 0xFF
	if err := a.Verify(); err == nil {
		t.Error("tampered signature should fail verification")
	}
}

func TestAnnounce_VerifyFailsOnWrongDestHash(t *testing.T) {
	id, _ := GenerateIdentity()
	a, _ := NewAnnounce(id, "meshsat.hub", nil)

	a.DestHash[0] ^= 0xFF
	if err := a.Verify(); err == nil {
		t.Error("wrong dest hash should fail verification")
	}
}

func TestAnnounce_VerifyFailsOnTamperedAppData(t *testing.T) {
	id, _ := GenerateIdentity()
	a, _ := NewAnnounce(id, "meshsat.hub", []byte("original"))

	a.AppData = []byte("tampered")
	if err := a.Verify(); err == nil {
		t.Error("tampered app data should fail verification")
	}
}

func TestAnnounce_PayloadMarshalRoundtrip(t *testing.T) {
	id, _ := GenerateIdentity()
	a1, _ := NewAnnounce(id, "meshsat.hub", []byte("test-data"))

	payload := a1.MarshalPayload()
	if len(payload) < AnnounceMinPayload {
		t.Fatalf("payload too short: %d < %d", len(payload), AnnounceMinPayload)
	}

	a2, err := UnmarshalAnnouncePayload(payload, a1.DestHash, a1.Hops, a1.ContextFlag)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(a2.PublicKey, a1.PublicKey) {
		t.Error("PublicKey mismatch")
	}
	if a2.NameHash != a1.NameHash {
		t.Error("NameHash mismatch")
	}
	if a2.Random != a1.Random {
		t.Error("Random mismatch")
	}
	if !bytes.Equal(a2.Signature, a1.Signature) {
		t.Error("Signature mismatch")
	}
	if !bytes.Equal(a2.AppData, a1.AppData) {
		t.Error("AppData mismatch")
	}

	// Verify the round-tripped announce
	if err := a2.Verify(); err != nil {
		t.Fatalf("round-tripped announce should verify: %v", err)
	}
}

func TestAnnounce_PacketMarshalRoundtrip(t *testing.T) {
	id, _ := GenerateIdentity()
	a1, _ := NewAnnounce(id, "meshsat.hub", []byte("app-data"))

	raw := a1.MarshalPacket()

	// Verify packet header
	h, err := UnmarshalHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if h.PacketType != PacketAnnounce {
		t.Errorf("PacketType: got %d, want %d", h.PacketType, PacketAnnounce)
	}
	if h.HeaderType != HeaderType1 {
		t.Errorf("HeaderType: got %d, want %d", h.HeaderType, HeaderType1)
	}
	if h.DestHash != a1.DestHash {
		t.Error("DestHash mismatch in header")
	}

	// Full packet unmarshal
	a2, err := UnmarshalAnnouncePacket(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := a2.Verify(); err != nil {
		t.Fatalf("packet round-tripped announce should verify: %v", err)
	}
	if !bytes.Equal(a2.AppData, a1.AppData) {
		t.Error("AppData mismatch after full packet roundtrip")
	}
}

func TestAnnounce_NoAppData(t *testing.T) {
	id, _ := GenerateIdentity()
	a, _ := NewAnnounce(id, "meshsat.hub", nil)

	payload := a.MarshalPayload()
	if len(payload) != AnnounceMinPayload {
		t.Errorf("payload size without app data: got %d, want %d", len(payload), AnnounceMinPayload)
	}

	a2, err := UnmarshalAnnouncePayload(payload, a.DestHash, a.Hops, a.ContextFlag)
	if err != nil {
		t.Fatal(err)
	}
	if len(a2.AppData) != 0 {
		t.Error("AppData should be nil/empty")
	}
	if err := a2.Verify(); err != nil {
		t.Fatalf("no-appdata announce should verify: %v", err)
	}
}

func TestUnmarshalAnnouncePayload_TooShort(t *testing.T) {
	_, err := UnmarshalAnnouncePayload(make([]byte, 10), [TruncatedHashLen]byte{}, 0, 0)
	if err == nil {
		t.Error("expected error for short payload")
	}
}

func TestUnmarshalAnnouncePacket_WrongType(t *testing.T) {
	// Build a DATA packet and try to parse as announce
	h := &Header{
		HeaderType: HeaderType1,
		PacketType: PacketData,
		Context:    ContextNone,
		Data:       make([]byte, 200),
	}
	raw := h.Marshal()
	_, err := UnmarshalAnnouncePacket(raw)
	if err == nil {
		t.Error("expected error for non-announce packet")
	}
}

func TestAnnounce_IncrementHop(t *testing.T) {
	id, _ := GenerateIdentity()
	a, _ := NewAnnounce(id, "meshsat.hub", nil)

	if a.Hops != 0 {
		t.Fatal("initial hops should be 0")
	}

	if !a.IncrementHop() {
		t.Error("first hop increment should succeed")
	}
	if a.Hops != 1 {
		t.Errorf("hops after increment: got %d, want 1", a.Hops)
	}

	// Still verifiable after hop increment (hops not in signature)
	if err := a.Verify(); err != nil {
		t.Fatalf("announce should verify after hop increment: %v", err)
	}

	// Max out hops
	a.Hops = PathfinderM
	if a.IncrementHop() {
		t.Error("should not increment past max hops")
	}
}

func TestAnnounce_UniqueRandom(t *testing.T) {
	id, _ := GenerateIdentity()
	a1, _ := NewAnnounce(id, "meshsat.hub", nil)
	a2, _ := NewAnnounce(id, "meshsat.hub", nil)

	if a1.Random == a2.Random {
		t.Error("two announces from same identity should have different randoms")
	}
}

func TestAnnounce_DestHashMatchesIdentity(t *testing.T) {
	id, _ := GenerateIdentity()
	appName := "meshsat.hub"

	a, _ := NewAnnounce(id, appName, nil)
	expected := id.DestHash(appName)

	if a.DestHash != expected {
		t.Error("announce DestHash should match identity.DestHash for same app name")
	}
}

func TestAnnounce_PublicKeyOrder(t *testing.T) {
	// Verify the public key is [X25519][Ed25519] per RNS spec
	id, _ := GenerateIdentity()
	a, _ := NewAnnounce(id, "meshsat.hub", nil)

	encBytes := id.EncryptionPublicKey().Bytes()
	sigBytes := id.SigningPublicKey()

	if !bytes.Equal(a.PublicKey[:EncryptionPubLen], encBytes) {
		t.Error("first 32 bytes of PublicKey should be X25519")
	}
	if !bytes.Equal(a.PublicKey[EncryptionPubLen:], sigBytes) {
		t.Error("last 32 bytes of PublicKey should be Ed25519")
	}
}

// TestAnnounce_RNSWireCompat validates the packet format against Python RNS's
// expected wire layout, field by field. This catches any mismatch in header
// packing, payload ordering, or hash computation that would cause RNS's
// Identity.validate_announce() to reject the packet.
func TestAnnounce_RNSWireCompat(t *testing.T) {
	id, _ := GenerateIdentity()
	appName := "meshsat.hub"
	appData := []byte("test-app-data")

	a, err := NewAnnounce(id, appName, appData)
	if err != nil {
		t.Fatal(err)
	}

	raw := a.MarshalPacket()

	// ── Header checks (RNS Packet.unpack) ──
	if len(raw) < HeaderMinSize {
		t.Fatalf("packet too short: %d < %d", len(raw), HeaderMinSize)
	}

	flags := raw[0]
	hops := raw[1]

	// Flags byte: [7:IFAC=0][6:HdrType=0][5:CtxFlag=0][4:TptType=0][3-2:DestType=00][1-0:PktType=01]
	wantFlags := byte(0x01) // ANNOUNCE with all other bits zero
	if flags != wantFlags {
		t.Errorf("flags: got 0x%02x, want 0x%02x", flags, wantFlags)
	}
	if flags&0x80 != 0 {
		t.Error("IFAC flag (bit 7) must not be set — RNS drops IFAC packets on non-IFAC interfaces")
	}
	if hops != 0 {
		t.Errorf("hops: got %d, want 0", hops)
	}

	// Dest hash (16 bytes at offset 2)
	destHash := raw[2:18]
	expectedDestHash := id.DestHash(appName)
	if !bytes.Equal(destHash, expectedDestHash[:]) {
		t.Error("dest hash in header doesn't match identity.DestHash()")
	}

	// Context byte
	ctx := raw[18]
	if ctx != ContextNone {
		t.Errorf("context: got 0x%02x, want 0x%02x (NONE)", ctx, ContextNone)
	}

	// ── Payload checks (RNS Identity.validate_announce field extraction) ──
	data := raw[19:]

	// Python RNS field offsets (no ratchet case):
	//   public_key = data[:64]
	//   name_hash  = data[64:74]
	//   random     = data[74:84]
	//   signature  = data[84:148]
	//   app_data   = data[148:]
	if len(data) < 148+len(appData) {
		t.Fatalf("payload too short: %d, expected at least %d", len(data), 148+len(appData))
	}

	pubKey := data[:64]
	nameHash := data[64:74]
	random := data[74:84]
	sig := data[84:148]
	payload := data[148:]

	// Public key: [32B X25519][32B Ed25519]
	if !bytes.Equal(pubKey[:32], id.EncryptionPublicKey().Bytes()) {
		t.Error("public key first 32 bytes should be X25519 encryption key")
	}
	if !bytes.Equal(pubKey[32:], id.SigningPublicKey()) {
		t.Error("public key last 32 bytes should be Ed25519 signing key")
	}

	// Name hash: SHA256(appName)[:10]
	expectedNameHash := sha256.Sum256([]byte(appName))
	if !bytes.Equal(nameHash, expectedNameHash[:NameHashLen]) {
		t.Errorf("name hash mismatch: got %x, want %x", nameHash, expectedNameHash[:NameHashLen])
	}

	// Random: [5 random][5 timestamp]
	ts := int64(random[5])<<32 | int64(binary.BigEndian.Uint32(random[6:]))
	now := time.Now().Unix()
	if ts < now-5 || ts > now+5 {
		t.Errorf("random timestamp not within 5s of now: got %d, now %d", ts, now)
	}

	// App data
	if !bytes.Equal(payload, appData) {
		t.Errorf("app data: got %q, want %q", payload, appData)
	}

	// ── Signature verification (same as RNS validate_announce) ──
	// signed_data = dest_hash + public_key + name_hash + random + app_data
	var signedData []byte
	signedData = append(signedData, destHash...)
	signedData = append(signedData, pubKey...)
	signedData = append(signedData, nameHash...)
	signedData = append(signedData, random...)
	signedData = append(signedData, payload...)

	if !VerifySignature(id.SigningPublicKey(), signedData, sig) {
		t.Error("signature verification failed using RNS signed_data layout")
	}

	// ── Destination hash verification (same as RNS validate_announce) ──
	// identity_hash = SHA256(enc_pub || sig_pub)[:16]
	idHasher := sha256.New()
	idHasher.Write(pubKey[:32])
	idHasher.Write(pubKey[32:])
	identityHash := idHasher.Sum(nil)[:TruncatedHashLen]

	// dest_hash = SHA256(name_hash || identity_hash)[:16]
	destHasher := sha256.New()
	destHasher.Write(nameHash)
	destHasher.Write(identityHash)
	computedDest := destHasher.Sum(nil)[:TruncatedHashLen]

	if !bytes.Equal(computedDest, destHash) {
		t.Errorf("destination hash mismatch: computed %x, header %x", computedDest, destHash)
	}

	// ── HDLC framing roundtrip ──
	frame := HDLCFrame(raw)
	if frame[0] != HDLCFlag || frame[len(frame)-1] != HDLCFlag {
		t.Error("HDLC frame missing flag bytes")
	}

	reader := NewHDLCFrameReader()
	frames := reader.Feed(frame)
	if len(frames) != 1 {
		t.Fatalf("HDLC roundtrip: expected 1 frame, got %d", len(frames))
	}
	if !bytes.Equal(frames[0], raw) {
		t.Error("HDLC roundtrip: frame content mismatch")
	}
}

// TestAnnounce_RandomTimestampFormat verifies the random hash embeds a valid
// timestamp in the last 5 bytes (RNS-compatible format).
func TestAnnounce_RandomTimestampFormat(t *testing.T) {
	id, _ := GenerateIdentity()
	before := time.Now().Unix()
	a, _ := NewAnnounce(id, "meshsat.hub", nil)
	after := time.Now().Unix()

	// Extract timestamp from last 5 bytes of Random
	ts := int64(a.Random[5])<<32 |
		int64(a.Random[6])<<24 |
		int64(a.Random[7])<<16 |
		int64(a.Random[8])<<8 |
		int64(a.Random[9])

	if ts < before || ts > after {
		t.Errorf("timestamp %d not in [%d, %d]", ts, before, after)
	}
}
