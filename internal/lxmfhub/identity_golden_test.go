package lxmfhub

import (
	"encoding/hex"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/interop/rnsenv"
	"github.com/meshsat/meshsat-hub/internal/lxmf"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
)

// The exported key is what RNS.Identity.from_bytes expects: Python derives
// the same identity hash and the same lxmf.delivery address from it, so a
// tenant can carry its Hub address into a client of its own.
func TestGolden_IdentityExportLoadsInPythonRNS(t *testing.T) {
	id, _ := rnswire.GenerateIdentity()
	var out struct {
		Identity string `json:"identity"`
		Delivery string `json:"delivery"`
	}
	rnsenv.RunJSON(t, &out, `
import sys, json, RNS
i = RNS.Identity.from_bytes(bytes.fromhex(sys.argv[1]))
print(json.dumps({"identity": i.hash.hex(), "delivery": RNS.Destination.hash(i, "lxmf", "delivery").hex()}))
`, PrivateKeyHex(id))
	ih := id.IdentityHash()
	dh := id.DestHash(lxmf.DeliveryName)
	if out.Identity != hex.EncodeToString(ih[:]) || out.Delivery != hex.EncodeToString(dh[:]) {
		t.Fatalf("python derived %+v, go has identity %x delivery %x", out, ih, dh)
	}
}
