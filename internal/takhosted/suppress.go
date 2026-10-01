package takhosted

import (
	"strings"
	"sync"
	"time"
)

// A node that speaks CoT for itself is not also drawn by the Hub (MESHSAT-1458).
//
// The forwarder turns every position a kit reports into a marker of its own,
// meshsat-device-<id>. A kit that exports CoT sends its own marker for the same
// node, under the UID it chose. Forward both and one radio is two dots on every
// map, a few metres apart, one of them named by an id.
//
// The node's own event is the better one: it carries the callsign, the team and
// the type the operator set. So when an exported position event is accepted, the
// node it describes is remembered until that event goes stale, and the Hub does
// not draw the same node from its position report in the meantime.
//
// By NODE, not by sender: a kit relays positions for mesh nodes it does not
// export, and suppressing everything a kit reports would take those off the map.
//
// It lives in the leader's memory and fails open. After a change of leader the
// new one has no marks, draws one Hub marker for a node that is exporting, and
// that marker fades when it goes stale two minutes later.

const (
	suppressMin = time.Minute
	suppressMax = 10 * time.Minute
)

// nodeKey reduces the ids one node goes by to one key: the device id on its
// position topic ("!aabbccdd"), the UID a kit exports for it ("MESHSAT-aabbccdd",
// "meshsat-aabbccdd") and the UID the Hub would draw ("meshsat-device-...").
func nodeKey(id string) string {
	k := strings.ToLower(strings.TrimSpace(id))
	k = strings.TrimPrefix(k, "meshsat-device-")
	k = strings.TrimPrefix(k, "meshsat-")
	k = strings.TrimPrefix(k, "!")
	return k
}

// suppressor remembers which nodes are exporting, per tenant.
type suppressor struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func newSuppressor() *suppressor { return &suppressor{until: map[string]time.Time{}} }

func suppressKey(tenantID, node string) string { return tenantID + "|" + node }

// mark records that this tenant's node has exported a position event that stays
// current until stale. The hold is clamped: a minute at least, so a kit whose
// events go stale faster than it reports does not flicker between the two
// markers, and ten at most, so a far-off stale time cannot take a node off the
// Hub's own drawing for good.
func (s *suppressor) mark(tenantID, uid string, stale, now time.Time) {
	node := nodeKey(uid)
	if node == "" {
		return
	}
	hold := stale.Sub(now)
	if hold < suppressMin {
		hold = suppressMin
	}
	if hold > suppressMax {
		hold = suppressMax
	}
	s.mu.Lock()
	s.until[suppressKey(tenantID, node)] = now.Add(hold)
	s.mu.Unlock()
}

// suppressed reports whether the Hub should leave this tenant's device undrawn.
func (s *suppressor) suppressed(tenantID, deviceID string, now time.Time) bool {
	node := nodeKey(deviceID)
	if node == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.until[suppressKey(tenantID, node)]
	return ok && now.Before(until)
}

func (s *suppressor) prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, until := range s.until {
		if !now.Before(until) {
			delete(s.until, k)
		}
	}
}

func (s *suppressor) forget(tenantID string) {
	prefix := tenantID + "|"
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.until {
		if strings.HasPrefix(k, prefix) {
			delete(s.until, k)
		}
	}
}
