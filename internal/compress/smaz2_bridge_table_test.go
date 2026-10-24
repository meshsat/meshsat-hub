package compress

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// The Meshtastic table is the Bridge's, verbatim (meshsat
// internal/compress/dict_meshtastic.go, Critical Rule 9). The two drifted at
// word 124 on 15 Apr 2026, when both removed "astrocast": the Hub wrote
// "globalstar", the Bridge a second "satellite". The Hub then read every
// Bridge's and app's "satellite" (word 124) as "globalstar", and they read
// the Hub's "globalstar" as "satellite". These fingerprints are the
// Bridge's; a change to the table here without the same change there fails.
// [MESHSAT-1675]
const (
	bridgeWordsSHA256   = "eeaa420383f289ec6779295f32f76db14cb9914e6b1fd340f7f2ab70ca16f09e"
	bridgeBigramsSHA256 = "1d35f1f1dfb026c94fa84b3e1c0976cc3913c8b80f838a0e879226875b626eed"
)

func TestMeshtasticTableIsTheBridges(t *testing.T) {
	words := sha256.Sum256([]byte(strings.Join(meshtasticWords, "\n")))
	if got := hex.EncodeToString(words[:]); got != bridgeWordsSHA256 {
		t.Errorf("word table fingerprint %s, the Bridge's is %s", got, bridgeWordsSHA256)
	}
	bigrams := sha256.Sum256([]byte(meshtasticBigrams))
	if got := hex.EncodeToString(bigrams[:]); got != bridgeBigramsSHA256 {
		t.Errorf("bigram fingerprint %s, the Bridge's is %s", got, bridgeBigramsSHA256)
	}
	if meshtasticWords[124] != "satellite" {
		t.Errorf("word 124 is %q, the Bridge's is \"satellite\"", meshtasticWords[124])
	}
}

// What the Bridge writes for "satellite" (word 124, the later of its two
// entries) reads back as "satellite" here, and the Hub writes it the same way.
func TestSatelliteIsTheBridgesWord124(t *testing.T) {
	bridge := []byte{0x07, 0x7c, 0x07, 0x02, 0x07, 0x83, 0x81, 0x65, 0x20, 0x48, 0x75, 0x62}
	got, err := Decompress(bridge)
	if err != nil || string(got) != "satellite message from the Hub" {
		t.Fatalf("Bridge bytes read as %q, %v", got, err)
	}
	if out := Compress([]byte("satellite message from the Hub")); string(out) != string(bridge) {
		t.Errorf("Hub writes % x, the Bridge writes % x", out, bridge)
	}
}
