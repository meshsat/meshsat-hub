package lxmf

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/interop/rnsenv"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
)

// A propagation stamp Go makes is one LXStamper.validate_pn_stamp accepts,
// with the same transient id and value; a stamp Python makes passes Go's
// check; and the propagation node announce Go packs is what
// LXMF.pn_announce_data_is_valid accepts and reads back.
func TestGolden_PropagationStampAndAnnounceMatchPython(t *testing.T) {
	// A message as a propagation node holds it: destination hash and bytes
	// that stand in for the encrypted payload.
	dest := make([]byte, DestLen)
	for i := range dest {
		dest[i] = byte(i)
	}
	lxm := append(append([]byte(nil), dest...), make([]byte, Overhead+40)...)
	for i := DestLen; i < len(lxm); i++ {
		lxm[i] = byte(i * 7)
	}
	stamp := GeneratePNStamp(lxm, 6)
	held, ok := ValidatePNStamp(append(append([]byte(nil), lxm...), stamp...), 6)
	if !ok || held.StampValue < 6 {
		t.Fatalf("own stamp rejected: ok %v value %d", ok, held.StampValue)
	}
	var out struct {
		TransientID string `json:"transient_id"`
		Value       int    `json:"value"`
		Valid       bool   `json:"valid"`
		PyStamp     string `json:"py_stamp"`
		Announce    []any  `json:"announce"`
		AnnounceOK  bool   `json:"announce_ok"`
	}
	ann := PropagationAnnounce{Timebase: 1760000000, Enabled: true, TransferLimitKB: 256, SyncLimitKB: 10240, StampCost: 16, StampCostFlex: 3, PeeringCost: 18, Name: "MeshSat test", Version: "test"}
	rnsenv.RunJSON(t, &out, `
import sys, json, RNS, LXMF
RNS.loglevel = 0
from LXMF import LXStamper
from LXMF.LXMF import pn_announce_data_is_valid
import RNS.vendor.umsgpack as msgpack
LXStamper.job_linux = LXStamper.job_simple
LXStamper.job_linux_managed = LXStamper.job_simple
lxm = bytes.fromhex(sys.argv[1]); go_stamp = bytes.fromhex(sys.argv[2]); cost = int(sys.argv[3])
tid, data, value, stamp = LXStamper.validate_pn_stamp(lxm + go_stamp, cost)
py_stamp, _ = LXStamper.generate_stamp(RNS.Identity.full_hash(lxm), cost, expand_rounds=LXStamper.WORKBLOCK_EXPAND_ROUNDS_PN)
app = bytes.fromhex(sys.argv[4])
ok = pn_announce_data_is_valid(app)
d = msgpack.unpackb(app)
d[6] = {str(k): (v.decode() if isinstance(v, bytes) else v) for k, v in d[6].items()}
print(json.dumps({"transient_id": tid.hex() if tid else "", "value": value or 0, "valid": tid is not None,
                  "py_stamp": py_stamp.hex(), "announce": d, "announce_ok": bool(ok)}))
`, hex.EncodeToString(lxm), hex.EncodeToString(stamp), "6", hex.EncodeToString(ann.AppData()))
	if !out.Valid || out.TransientID != hex.EncodeToString(held.TransientID[:]) || out.Value != held.StampValue {
		t.Fatalf("python judged Go's stamp: %+v, go value %d id %x", out, held.StampValue, held.TransientID)
	}
	pyStamp, _ := hex.DecodeString(out.PyStamp)
	if _, ok := ValidatePNStamp(append(append([]byte(nil), lxm...), pyStamp...), 6); !ok {
		t.Fatal("Go rejected Python's propagation stamp")
	}
	if !out.AnnounceOK {
		t.Fatalf("python rejected the propagation announce: %v", out.Announce)
	}
	b, _ := json.Marshal(out.Announce)
	t.Logf("python read the announce as %s", b)
	parsed, err := ParsePropagationAnnounce(ann.AppData())
	if err != nil || parsed.StampCost != 16 || parsed.Name != "MeshSat test" || !parsed.Enabled || parsed.TransferLimitKB != 256 {
		t.Fatalf("own announce parse: %+v %v", parsed, err)
	}
	_ = rnswire.MTU
}
