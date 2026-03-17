package compress

import "testing"

func TestRoundTrip_CommonStrings(t *testing.T) {
	tests := []string{
		"Hello, World!",
		"the quick brown fox jumps over the lazy dog",
		"Battery level 85 percent signal strength position latitude longitude",
		"emergency rescue medical assistance helicopter",
		"mesh node gateway relay bridge network",
		"satellite iridium modem connected online status",
		"SOS all clear moving to checkpoint B",
		"",
	}
	for _, input := range tests {
		compressed := CompressString(input)
		decompressed, err := DecompressString(compressed)
		if err != nil {
			t.Fatalf("Decompress(%q) error: %v", input, err)
		}
		if decompressed != input {
			t.Errorf("round-trip failed: got %q, want %q", decompressed, input)
		}
	}
}

func TestRoundTrip_SingleBytes(t *testing.T) {
	for b := 0; b < 256; b++ {
		input := []byte{byte(b)}
		compressed := Compress(input)
		decompressed, err := Decompress(compressed)
		if err != nil {
			t.Fatalf("byte 0x%02x: decompress error: %v", b, err)
		}
		if len(decompressed) != 1 || decompressed[0] != byte(b) {
			t.Errorf("byte 0x%02x: round-trip failed: got %v", b, decompressed)
		}
	}
}

func TestCompress_MeshtasticTermsCompressWell(t *testing.T) {
	input := []byte("battery level 85 percent signal strength position latitude longitude altitude heading speed satellite iridium gateway mesh node relay bridge network")
	compressed := Compress(input)
	ratio := float64(len(compressed)) / float64(len(input))
	t.Logf("Meshtastic text: %d -> %d bytes (%.1f%%)", len(input), len(compressed), ratio*100)
	if ratio > 0.60 {
		t.Errorf("compression ratio %.2f exceeds 60%% threshold for Meshtastic text", ratio)
	}
}

func TestDecompress_NilInput(t *testing.T) {
	result, err := Decompress(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestCompress_NilInput(t *testing.T) {
	result := Compress(nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestRoundTrip_WordBoundaries(t *testing.T) {
	tests := []string{
		"meshwork",
		"gateway2",
		"node123",
		"A",
		"ab",
		"abc",
	}
	for _, input := range tests {
		compressed := CompressString(input)
		decompressed, err := DecompressString(compressed)
		if err != nil {
			t.Fatalf("Decompress(%q) error: %v", input, err)
		}
		if decompressed != input {
			t.Errorf("round-trip failed for %q: got %q", input, decompressed)
		}
	}
}

func BenchmarkCompress(b *testing.B) {
	data := []byte("Battery level 85 percent, signal strength good, position latitude 52.3 longitude 4.9, heading north")
	for i := 0; i < b.N; i++ {
		Compress(data)
	}
}

func BenchmarkDecompress(b *testing.B) {
	data := []byte("Battery level 85 percent, signal strength good, position latitude 52.3 longitude 4.9, heading north")
	compressed := Compress(data)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Decompress(compressed)
	}
}
