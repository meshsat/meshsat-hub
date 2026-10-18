package hembfixtures

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The release the other repos lock to: every file matches SHA256SUMS, the
// folder holds nothing SHA256SUMS does not list, and SHA256SUMS is the one
// recorded here. A changed fixture is a new release with a new tag, never an
// edit of this one. [MESHSAT-1584]
const (
	release    = "fixtures-v1.1"
	sumsSHA256 = "433684467695e3061e41157276e50c62e57e00eb13aeb7e55c8c4c8ba499e91d"
)

func TestTheFixtureReleaseIsIntact(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "fixtures", "v1")
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if got := sha256.Sum256(sums); hex.EncodeToString(got[:]) != sumsSHA256 {
		t.Fatalf("SHA256SUMS is %x, the release recorded %s: a fixture changed without a new release", got, sumsSHA256)
	}
	listed := map[string]bool{"SHA256SUMS": true}
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		want, name, ok := strings.Cut(sc.Text(), "  ")
		if !ok || strings.ContainsAny(name, "/\\") {
			t.Fatalf("bad SHA256SUMS line %q", sc.Text())
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
			t.Errorf("%s differs from SHA256SUMS", name)
		}
		listed[name] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !listed[e.Name()] {
			t.Errorf("%s is in the release folder but not in SHA256SUMS", e.Name())
		}
	}
	if r, err := os.ReadFile(filepath.Join(dir, "RELEASE")); err != nil || string(r) != release+"\n" {
		t.Errorf("RELEASE is %q, want %q", r, release)
	}
}
