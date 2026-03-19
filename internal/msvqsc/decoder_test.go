package msvqsc

import (
	"os"
	"path/filepath"
	"testing"
)

func findAssets(t *testing.T) (string, string) {
	t.Helper()
	// Try relative paths from repo root.
	bases := []string{
		"assets/msvqsc",
		"../../assets/msvqsc",
		"../../../assets/msvqsc",
	}
	for _, base := range bases {
		cb := filepath.Join(base, "codebook_v1.bin")
		ci := filepath.Join(base, "corpus_index.bin")
		if _, err := os.Stat(cb); err == nil {
			return cb, ci
		}
	}
	t.Skip("MSVQ-SC assets not found (codebook_v1.bin, corpus_index.bin)")
	return "", ""
}

func TestLoad(t *testing.T) {
	cb, ci := findAssets(t)
	d, err := Load(cb, ci)
	if err != nil {
		t.Fatal(err)
	}
	stages, k, dim, corpusSize := d.Stats()
	t.Logf("Loaded: stages=%d, K=%d, dim=%d, corpus=%d entries", stages, k, dim, corpusSize)

	if stages < 1 || stages > 8 {
		t.Errorf("stages = %d, want 1-8", stages)
	}
	if k < 1 {
		t.Errorf("K = %d, want > 0", k)
	}
	if dim < 1 {
		t.Errorf("dim = %d, want > 0", dim)
	}
	if corpusSize < 1 {
		t.Errorf("corpus = %d, want > 0", corpusSize)
	}
}

func TestLooksLikeMSVQSC(t *testing.T) {
	// Valid: 3 stages, version 1 → header = 0x31
	valid := []byte{0x31, 0x00, 0x00, 0x01, 0x00, 0x02, 0x00} // 3 stages × 2 bytes = 6 + 1 header = 7
	if !LooksLikeMSVQSC(valid) {
		t.Error("expected valid MSVQ-SC")
	}

	// Wrong version.
	wrongVer := []byte{0x32, 0x00, 0x00, 0x01, 0x00, 0x02, 0x00}
	if LooksLikeMSVQSC(wrongVer) {
		t.Error("expected false for wrong version")
	}

	// Wrong length.
	short := []byte{0x31, 0x00}
	if LooksLikeMSVQSC(short) {
		t.Error("expected false for wrong length")
	}

	// Empty.
	if LooksLikeMSVQSC(nil) {
		t.Error("expected false for nil")
	}

	// Not MSVQ-SC (JSON).
	if LooksLikeMSVQSC([]byte(`{"text":"hello"}`)) {
		t.Error("expected false for JSON")
	}
}

func TestDecode_WithRealAssets(t *testing.T) {
	cb, ci := findAssets(t)
	d, err := Load(cb, ci)
	if err != nil {
		t.Fatal(err)
	}

	// Create a wire payload: 3 stages, indices [0, 0, 0] (first codebook entry at each stage).
	wire := []byte{
		0x31,       // header: 3 stages, version 1
		0x00, 0x00, // index 0
		0x00, 0x00, // index 0
		0x00, 0x00, // index 0
	}

	text, err := d.Decode(wire)
	if err != nil {
		t.Fatal(err)
	}
	if text == "" {
		t.Error("expected non-empty decoded text")
	}
	t.Logf("Decoded [0,0,0] → %q", text)
}

func TestDecode_InvalidVersion(t *testing.T) {
	cb, ci := findAssets(t)
	d, err := Load(cb, ci)
	if err != nil {
		t.Fatal(err)
	}

	wire := []byte{0x32, 0x00, 0x00} // version 2, not supported
	_, err = d.Decode(wire)
	if err == nil {
		t.Error("expected error for unsupported version")
	}
}
