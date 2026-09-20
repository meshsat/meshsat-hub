package fragment

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/codec"
)

// kitWire builds a payload with the shape that exposed MESHSAT-1280: the
// protocol version byte 0x01 followed by the base64 text of 301 bytes of
// ciphertext-like data (12-byte nonce + body + 16-byte tag), 405 bytes in all.
// The bytes are synthetic. No real traffic or device identity belongs in a test.
func kitWire() []byte {
	body := make([]byte, 301)
	x := uint32(0x9E3779B9)
	for i := range body {
		x = x*1664525 + 1013904223
		body[i] = byte(x >> 24)
	}
	return append([]byte{0x01}, []byte(base64.StdEncoding.EncodeToString(body))...)
}

func TestVersionByteIsPinnedToCodec(t *testing.T) {
	if versionByte != codec.ProtoVersion1 {
		t.Fatalf("fragment.versionByte = %#x, codec.ProtoVersion1 = %#x", versionByte, codec.ProtoVersion1)
	}
}

func TestClaimsLeavesWholeMessagesAlone(t *testing.T) {
	real := kitWire()
	if len(real) != 405 {
		t.Fatalf("kitWire is %d bytes, want 405", len(real))
	}
	if !IsFragment(real) {
		t.Fatal("premise gone: IsFragment no longer misreads this payload, so this test proves nothing")
	}
	_, stripped := codec.StripVersionByte(real)
	envelope := []byte(`{"from":1111111111,"to":4294967295,"channel":0,"id":2222222222,"portnum":1,"portnum_name":"TEXT_MESSAGE_APP","decoded_text":"hello from the field"}`)
	// A DTN bundle fragment from the Bridge, cut to a full SBD frame: version
	// byte, then the bundle header's own 0x01, exactly one MTU long. This is
	// the shape that makes "0x01 always means version" necessary: it passes
	// the length rule.
	bundle := append([]byte{0x01, 0x01}, bytes.Repeat([]byte{0xAB}, IridiumMO_MTU-2)...)

	for _, tc := range []struct {
		name string
		data []byte
		mtu  int
	}{
		{"a kit message, on IMT", real, 0},
		{"the same message, had it come over SBD", real, IridiumMO_MTU},
		{"the same bytes with the version byte already gone", stripped, IridiumMO_MTU},
		{"an untransformed JSON envelope", envelope, IridiumMO_MTU},
		{"a versioned DTN bundle fragment of exactly one MTU", bundle, IridiumMO_MTU},
		{"an encrypted SMS: random first byte, 110 bytes", append([]byte{0x13}, bytes.Repeat([]byte{0x77}, 109)...), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReassembler(time.Minute)
			if r.Claims("300000000000001", tc.data, tc.mtu) {
				i, n, id := DecodeHeader(tc.data[0], tc.data[1])
				t.Fatalf("claimed as fragment %d of %d (msg id %d); it is a whole message and would never be processed", i+1, n, id)
			}
		})
	}
}

// What the scheme exists for still works: a message cut by Fragment is claimed
// piece by piece, in order, and comes back whole.
func TestClaimsStillReassemblesARealSplit(t *testing.T) {
	msg := bytes.Repeat([]byte("field report "), 60) // 780 bytes -> 3 fragments
	frags := Fragment(msg, IridiumMO_MTU, 0x42)
	if len(frags) != 3 {
		t.Fatalf("want 3 fragments, got %d", len(frags))
	}
	r := NewReassembler(time.Minute)
	var out []byte
	for i, f := range frags {
		if !r.Claims("dev", f, IridiumMO_MTU) {
			t.Fatalf("fragment %d of a real split was not claimed", i+1)
		}
		got, err := r.AddFragment("dev", f)
		if err != nil {
			t.Fatal(err)
		}
		out = got
	}
	if !bytes.Equal(out, msg) {
		t.Fatalf("reassembled %d bytes, want %d", len(out), len(msg))
	}
}

// A tail with no siblings waiting is not claimed: processed as a whole message
// it is wrong but visible, parked it would be wrong and silent.
func TestClaimsRefusesAnOrphanTail(t *testing.T) {
	frags := Fragment(bytes.Repeat([]byte("x"), 600), IridiumMO_MTU, 7)
	r := NewReassembler(time.Minute)
	if r.Claims("dev", frags[len(frags)-1], IridiumMO_MTU) {
		t.Fatal("an orphan tail was claimed")
	}
	// ... nor for another device's siblings.
	_, _ = r.AddFragment("other", frags[0])
	if r.Claims("dev", frags[len(frags)-1], IridiumMO_MTU) {
		t.Fatal("a tail was claimed on the strength of another device's fragments")
	}
}
