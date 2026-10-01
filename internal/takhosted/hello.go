package takhosted

import (
	"time"

	"github.com/meshsat/meshsat-hub/internal/tak"
)

// Who the Hub says it is on a connection to a TAK server (MESHSAT-1461).
//
// # Why it has to say anything
//
// A TAK server does not send a connection anything until that connection has
// identified itself. In OpenTAKServer 1.7.13 (eud_handler/EudHandler.py) the
// queues a connection reads from are declared in parse_device_info, which runs
// for the FIRST event that carries a <contact>: before that the connection can
// publish and will never be sent a thing. The same code takes the UID of that
// first event as the connection's own, and uses it to decide what NOT to send
// back: `if body["uid"] != self.uid`.
//
// Until now the first such event was whichever device position happened to be
// forwarded first, so a connection was, by accident, "that device". Now the Hub
// introduces itself before anything else is written, which makes two things
// true on purpose: the server starts sending as soon as the connection is up,
// whether or not any kit has reported yet, and the connection's identity is the
// Hub's, so nothing the Hub writes on it is echoed back.
//
// # Why it is not drawn
//
// It is not a position, so it is not given a position type. A TAK client draws
// atoms ("a-...") and a few named bits; a type it has no handler for is dropped.
// The point is 0,0 with the "unknown" error values because an event must have a
// point, not because the Hub is anywhere.
//
// # The ping
//
// Its UID ends in "ping", which is how OpenTAKServer recognises one and keeps it
// from being taken for an identity. A server answers a ping; whatever it answers
// is a control type and is dropped on the way back in.

const (
	// hubUIDPrefix starts every UID the Hub uses for itself on a TAK stream.
	hubUIDPrefix = "meshsat-hub-"

	hubCallsign  = "MeshSat Hub"
	hubHelloType = "t-x-meshsat-hub"
	hubPingUID   = hubUIDPrefix + "ping"

	// unknownError is CoT's "no estimate" for a point's error values.
	unknownError = 9999999.0
)

// hubHello is the event the Hub writes first on every connection.
func hubHello(tenantID string, now time.Time) []byte {
	return hubEvent(hubUIDPrefix+tenantID, hubHelloType, now, time.Minute,
		&tak.CotDetail{Contact: &tak.CotContact{Callsign: hubCallsign}})
}

// hubPing is the keepalive.
func hubPing(now time.Time) []byte {
	return hubEvent(hubPingUID, tak.TypeKeepalive, now, 20*time.Second, nil)
}

func hubEvent(uid, typ string, now time.Time, life time.Duration, detail *tak.CotDetail) []byte {
	const layout = "2006-01-02T15:04:05Z"
	ts := now.UTC().Format(layout)
	xml, err := tak.MarshalCotEvent(tak.CotEvent{
		Version: "2.0", UID: uid, Type: typ, How: "h-g-i-g-o",
		Time: ts, Start: ts, Stale: now.UTC().Add(life).Format(layout),
		Point:  tak.CotPoint{Ce: unknownError, Le: unknownError},
		Detail: detail,
	})
	if err != nil {
		// A struct of strings and floats does not fail to marshal. If it ever
		// did, writing nothing is right: a connection with no hello still
		// forwards.
		return nil
	}
	return append(xml, '\n')
}
