package warnings

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// A digest goes only to a bridge that can do something with it: a MeshSat
// Bridge whose last birth declares a bearer the relay puts a warning on.
// The Bridge names each interface by its gateway type, so these are the
// types a kit's birth carries for the mesh, the APRS gateway and the 2G
// cell. A phone registered as a bridge (birth mode android or ios) declares
// a "meshtastic" interface too, its BLE node, but the apps hold no relay:
// a digest to a phone is a message nobody reads, and a kit that has not
// sent a birth has declared nothing yet.
var relayBearers = map[string]string{
	"meshtastic": "mesh",
	"aprs":       "APRS",
	"cell":       "2G cell",
}

// CanRelay reports whether a bridge takes warning digests, with the reason
// when it does not.
func CanRelay(b *store.Bridge) (bool, string) {
	if b == nil {
		return false, "no bridge"
	}
	if strings.TrimSpace(b.LastBirth) == "" {
		return false, "no birth yet"
	}
	var birth protocol.BridgeBirth
	if err := json.Unmarshal([]byte(b.LastBirth), &birth); err != nil {
		return false, "unreadable birth"
	}
	if protocol.PhoneMode(birth.Mode) {
		return false, "a phone"
	}
	for _, iface := range birth.Interfaces {
		if _, ok := relayBearers[strings.ToLower(iface.Type)]; ok {
			return true, ""
		}
	}
	return false, "no mesh, APRS or 2G cell interface"
}

// relayCapable splits a tenant's bridges into the kits a digest may go to
// and the rest, each with its reason, sorted by id.
func relayCapable(bridges []*store.Bridge) (kits []*store.Bridge, skipped []string) {
	for _, b := range bridges {
		if b == nil {
			continue
		}
		if ok, why := CanRelay(b); ok {
			kits = append(kits, b)
		} else {
			skipped = append(skipped, b.BridgeID+": "+why)
		}
	}
	sort.Strings(skipped)
	return kits, skipped
}
