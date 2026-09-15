// Package relay is the Hub side of the WebSocket relay (MESHSAT-612): a
// bridge that cannot be reached directly (behind carrier NAT, on a phone, on
// a satellite bearer) opens one outbound WebSocket to the Hub and serves its
// clients through it. A client opens its own WebSocket to the Hub naming the
// bridge, and the Hub joins the two. Both ends are bridges of the same tenant
// and authenticate with the MQTT credentials they already hold.
//
// The Hub never looks inside a frame. What travels is the ciphertext of an
// mTLS session the two ends negotiate with each other through the tunnel, so
// a compromised Hub can withhold or delay traffic but not read or forge it.
//
// The two ends of a tunnel land on different replicas behind round-robin, so
// the join is a rendezvous over the message bus rather than a map on one pod:
// every replica subscribes to the relay topics once and dispatches from its
// own in-memory sessions; a replica holding neither end ignores the frame.
package relay

import (
	"errors"
	"fmt"
)

// envelopeVersion is the first byte of every frame on the BRIDGE socket. The
// client socket carries bare payloads: a client is one tunnel, a bridge
// socket multiplexes all of them and needs to know which.
const envelopeVersion = 0x01

// MaxClientID bounds the client id in an envelope: one length byte.
const MaxClientID = 255

var (
	// ErrEnvelope is returned for a bridge-socket frame that does not parse.
	ErrEnvelope = errors.New("relay: malformed envelope")
	// ErrClientID is returned for a client id an envelope cannot carry.
	ErrClientID = errors.New("relay: client id empty or longer than 255 bytes")
)

// EncodeEnvelope frames payload for the bridge socket:
//
//	[0x01][len u8][client_id][payload]
//
// The payload may be empty (a keepalive of the inner protocol is still a frame).
func EncodeEnvelope(clientID string, payload []byte) ([]byte, error) {
	if clientID == "" || len(clientID) > MaxClientID {
		return nil, ErrClientID
	}
	out := make([]byte, 0, 2+len(clientID)+len(payload))
	out = append(out, envelopeVersion, byte(len(clientID))) // #nosec G115 -- bounded to MaxClientID above
	out = append(out, clientID...)
	out = append(out, payload...)
	return out, nil
}

// DecodeEnvelope reverses EncodeEnvelope. The returned payload aliases frame.
func DecodeEnvelope(frame []byte) (clientID string, payload []byte, err error) {
	if len(frame) < 2 {
		return "", nil, fmt.Errorf("%w: %d bytes", ErrEnvelope, len(frame))
	}
	if frame[0] != envelopeVersion {
		return "", nil, fmt.Errorf("%w: version 0x%02x", ErrEnvelope, frame[0])
	}
	n := int(frame[1])
	if n == 0 || len(frame) < 2+n {
		return "", nil, fmt.Errorf("%w: client id length %d in %d bytes", ErrEnvelope, n, len(frame))
	}
	return string(frame[2 : 2+n]), frame[2+n:], nil
}
