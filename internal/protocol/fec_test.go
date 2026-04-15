package protocol

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestFECRoundTrip(t *testing.T) {
	data := []byte("Hello from the MeshSat Hub FEC test!")
	encoded, err := FECEncode(data, 4, 2)
	if err != nil {
		t.Fatalf("FECEncode: %v", err)
	}
	decoded, err := FECDecode(encoded)
	if err != nil {
		t.Fatalf("FECDecode: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Fatalf("round-trip mismatch: got %q, want %q", decoded, data)
	}
}

func TestFECRoundTripWithInterleave(t *testing.T) {
	data := []byte("FEC v2 with interleaving test payload for LoRa burst protection.")
	opts := FECEncodeOpts{Interleave: true, InterleaveDepth: 8}
	encoded, err := FECEncode(data, 4, 2, opts)
	if err != nil {
		t.Fatalf("FECEncode v2: %v", err)
	}

	// Verify v2 header.
	if encoded[0] != FECVersion2 {
		t.Fatalf("expected version 0x%02x, got 0x%02x", FECVersion2, encoded[0])
	}
	flags := encoded[5]
	if flags&fecFlagInterleaved == 0 {
		t.Fatal("interleave flag not set")
	}
	depthEnc := int((flags>>1)&0x0F) + 1
	if depthEnc != 8 {
		t.Fatalf("expected depth 8, got %d", depthEnc)
	}

	decoded, err := FECDecode(encoded)
	if err != nil {
		t.Fatalf("FECDecode v2: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Fatal("v2 round-trip mismatch")
	}
}

func TestFECErasureRecovery(t *testing.T) {
	data := make([]byte, 200)
	rand.Read(data)

	encoded, err := FECEncode(data, 4, 2)
	if err != nil {
		t.Fatalf("FECEncode: %v", err)
	}

	// Erase 2 shards (the maximum recoverable with m=2).
	recovered, n, err := FECDecodeWithErasures(encoded, []int{0, 3})
	if err != nil {
		t.Fatalf("FECDecodeWithErasures: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 recovered shards, got %d", n)
	}
	if !bytes.Equal(recovered, data) {
		t.Fatal("erasure recovery mismatch")
	}
}

func TestFECInterleaveRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		size  int
		depth int
	}{
		{"9 bytes depth 3", 9, 3},
		{"12 bytes depth 4", 12, 4},
		{"100 bytes depth 8", 100, 8},
		{"255 bytes depth 5", 255, 5},
		{"1000 bytes depth 8", 1000, 8},
		{"7 bytes depth 3", 7, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, tc.size)
			for i := range data {
				data[i] = byte(i % 256)
			}
			interleaved := FECInterleave(data, tc.depth)
			if len(interleaved) != len(data) {
				t.Fatalf("interleave changed length: %d -> %d", len(data), len(interleaved))
			}
			deinterleaved := FECDeinterleave(interleaved, tc.depth)
			if !bytes.Equal(deinterleaved, data) {
				t.Fatalf("round-trip mismatch")
			}
		})
	}
}

func TestFECInterleaveNoopCases(t *testing.T) {
	data := []byte{1, 2, 3, 4, 5}

	result := FECInterleave(data, 0)
	if !bytes.Equal(result, data) {
		t.Fatal("depth 0 should be no-op")
	}

	result = FECInterleave(data, 1)
	if !bytes.Equal(result, data) {
		t.Fatal("depth 1 should be no-op")
	}
}

func TestFECAllInterleaveDepths(t *testing.T) {
	data := make([]byte, 200)
	rand.Read(data)

	for depth := 1; depth <= 16; depth++ {
		opts := FECEncodeOpts{Interleave: true, InterleaveDepth: depth}
		encoded, err := FECEncode(data, 4, 2, opts)
		if err != nil {
			t.Fatalf("depth %d: encode: %v", depth, err)
		}
		decoded, err := FECDecode(encoded)
		if err != nil {
			t.Fatalf("depth %d: decode: %v", depth, err)
		}
		if !bytes.Equal(decoded, data) {
			t.Fatalf("depth %d: round-trip mismatch", depth)
		}
	}
}

func TestFECInvalidInputs(t *testing.T) {
	if _, err := FECEncode(nil, 0, 2); err == nil {
		t.Fatal("expected error for data_shards=0")
	}
	if _, err := FECEncode(nil, 2, 0); err == nil {
		t.Fatal("expected error for parity_shards=0")
	}
	if _, err := FECDecode(nil); err == nil {
		t.Fatal("expected error for nil data")
	}
	if _, err := FECDecode([]byte{0xFF, 0, 0, 0, 0}); err == nil {
		t.Fatal("expected error for bad version")
	}
}

func TestFECProfileLookup(t *testing.T) {
	knownTypes := []string{
		"lora", "mesh", "sbd", "iridium", "imt", "iridium_imt",
		"ax25", "tcp", "mqtt", "webhook",
		"cellular", "sms", "zigbee",
	}
	for _, ct := range knownTypes {
		p, ok := LookupFECProfile(ct)
		if !ok {
			t.Errorf("missing profile for %q", ct)
			continue
		}
		switch ct {
		case "tcp", "mqtt", "webhook":
			if p.DataShards != 0 || p.ParityShards != 0 {
				t.Errorf("%q should have no FEC, got k=%d m=%d", ct, p.DataShards, p.ParityShards)
			}
		default:
			if p.DataShards < 1 || p.ParityShards < 1 {
				t.Errorf("%q has invalid FEC params k=%d m=%d", ct, p.DataShards, p.ParityShards)
			}
		}
	}

	_, ok := LookupFECProfile("unknown_type")
	if ok {
		t.Error("expected not-found for unknown type")
	}
}

func TestResolveFECParamsNamedProfile(t *testing.T) {
	params := map[string]string{"profile": "lora"}
	ds, ps, il, ild := ResolveFECParams(params)
	if ds != 4 || ps != 2 {
		t.Fatalf("lora profile: expected k=4 m=2, got k=%d m=%d", ds, ps)
	}
	if !il || ild != 8 {
		t.Fatalf("lora profile: expected interleave=true depth=8, got %v %d", il, ild)
	}
}

func TestResolveFECParamsAutoProfile(t *testing.T) {
	params := map[string]string{"profile": "auto", "channel": "ax25"}
	ds, ps, il, ild := ResolveFECParams(params)
	if ds != 4 || ps != 3 {
		t.Fatalf("ax25 auto: expected k=4 m=3, got k=%d m=%d", ds, ps)
	}
	if !il || ild != 16 {
		t.Fatalf("ax25 auto: expected interleave=true depth=16, got %v %d", il, ild)
	}
}

func TestResolveFECParamsNoFEC(t *testing.T) {
	params := map[string]string{"profile": "tcp"}
	ds, ps, _, _ := ResolveFECParams(params)
	if ds != 0 || ps != 0 {
		t.Fatalf("tcp profile: expected k=0 m=0, got k=%d m=%d", ds, ps)
	}
}

func TestResolveFECParamsDefault(t *testing.T) {
	params := map[string]string{}
	ds, ps, il, _ := ResolveFECParams(params)
	if ds != 4 || ps != 2 {
		t.Fatalf("default: expected k=4 m=2, got k=%d m=%d", ds, ps)
	}
	if il {
		t.Fatal("default should not interleave")
	}
}

func TestAdaptFECProfile(t *testing.T) {
	base := FECProfile{DataShards: 4, ParityShards: 2, Interleave: true, InterleaveDepth: 8}

	adapted := AdaptFECProfile(base, 90)
	if adapted.ParityShards != 2 {
		t.Fatalf("healthy: expected m=2, got m=%d", adapted.ParityShards)
	}

	adapted = AdaptFECProfile(base, 60)
	if adapted.ParityShards != 3 {
		t.Fatalf("degraded: expected m=3, got m=%d", adapted.ParityShards)
	}

	adapted = AdaptFECProfile(base, 30)
	if adapted.ParityShards != 4 {
		t.Fatalf("poor: expected m=4, got m=%d", adapted.ParityShards)
	}

	noFEC := FECProfile{DataShards: 0, ParityShards: 0}
	adapted = AdaptFECProfile(noFEC, 10)
	if adapted.DataShards != 0 || adapted.ParityShards != 0 {
		t.Fatal("no-FEC profile should not change")
	}

	adapted = AdaptFECProfile(base, 40)
	if !adapted.Interleave || adapted.InterleaveDepth != 8 {
		t.Fatal("interleave settings should be preserved")
	}
}

func TestParseIntParam(t *testing.T) {
	params := map[string]string{"a": "42", "b": "0", "c": "abc"}
	if v := ParseIntParam(params, "a", 0); v != 42 {
		t.Fatalf("expected 42, got %d", v)
	}
	if v := ParseIntParam(params, "missing", 99); v != 99 {
		t.Fatalf("expected 99, got %d", v)
	}
	if v := ParseIntParam(params, "c", 7); v != 7 {
		t.Fatalf("expected 7 for non-numeric, got %d", v)
	}
}
