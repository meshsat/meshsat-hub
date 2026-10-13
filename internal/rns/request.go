package rns

import (
	"crypto/sha256"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/msgpack"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
)

// Requests and responses over a link (RNS.Link.request / handle_request).
//
// A request is msgpack [requested_at, path_hash, data]; it travels as one
// link DATA packet with context REQUEST when it fits the link's MDU, else as
// a resource whose advertisement carries the request id and the request
// flag. Its id is the packet's truncated hash, or the truncated hash of the
// packed request when it went as a resource. A response is msgpack
// [request_id, data], as a RESPONSE packet or as a resource with the request
// id and the response flag. LXMF propagation nodes serve "/get" and
// "/offer" this way (MESHSAT-1555).

// Request is one request a peer made on a link to a local destination.
type Request struct {
	Path     string
	PathHash [HashLen]byte
	ID       [HashLen]byte
	Data     msgpack.Value
	Link     *Link
	// RemoteIdentity is the identity the peer identified with on the link,
	// nil when it did not.
	RemoteIdentity *rnswire.Identity
	RequestedAt    float64
}

// RequestHandler answers a request; ok false sends nothing back (upstream's
// None response).
type RequestHandler func(req *Request) (msgpack.Value, bool)

type requestEntry struct {
	path string
	fn   RequestHandler
}

// PathHash is RNS.Identity.truncated_hash(path): the first 16 bytes of
// SHA-256 over the path.
func PathHash(path string) [HashLen]byte {
	h := sha256.Sum256([]byte(path))
	var out [HashLen]byte
	copy(out[:], h[:HashLen])
	return out
}

// RegisterRequestHandler serves path on links to this destination.
func (d *Destination) RegisterRequestHandler(path string, fn RequestHandler) {
	d.ratchetMu.Lock()
	defer d.ratchetMu.Unlock()
	if d.requestHandlers == nil {
		d.requestHandlers = map[[HashLen]byte]*requestEntry{}
	}
	d.requestHandlers[PathHash(path)] = &requestEntry{path: path, fn: fn}
}

func (d *Destination) requestHandler(h [HashLen]byte) *requestEntry {
	d.ratchetMu.Lock()
	defer d.ratchetMu.Unlock()
	return d.requestHandlers[h]
}

func (d *Destination) hasRequestHandlers() bool {
	d.ratchetMu.Lock()
	defer d.ratchetMu.Unlock()
	return len(d.requestHandlers) > 0
}

// RequestResult is the outcome of a request this node made.
type RequestResult struct {
	Response msgpack.Value
	Err      error
}

// RequestReceipt tracks one outstanding request.
type RequestReceipt struct {
	ID   [HashLen]byte
	link *Link
	done chan RequestResult
}

// Done resolves with the response, or an error on timeout or link loss.
func (r *RequestReceipt) Done() <-chan RequestResult { return r.done }

// ErrRequestTimeout: no response within the timeout.
var ErrRequestTimeout = errors.New("rns: request timed out")

// Request sends a request on an active link and returns its receipt. timeout
// 0 derives one from the link's RTT as upstream does.
func (m *LinkManager) Request(l *Link, path string, data msgpack.Value, timeout time.Duration) (*RequestReceipt, error) {
	if l.State() != LinkActive && l.State() != LinkStale {
		return nil, errors.New("rns: link not active")
	}
	now := m.n.now()
	ph := PathHash(path)
	packed := msgpack.Pack(msgpack.Array(msgpack.Float(float64(now.UnixNano())/1e9), msgpack.Bin(ph[:]), data))
	if timeout <= 0 {
		// Link.TRAFFIC_TIMEOUT_FACTOR * rtt + RESPONSE_MAX_GRACE_TIME * 1.125
		timeout = 6*l.RTT() + 11250*time.Millisecond
		if timeout < 10*time.Second {
			timeout = 10 * time.Second
		}
	}
	rec := &RequestReceipt{link: l, done: make(chan RequestResult, 1)}
	if len(packed) <= l.MDU() {
		l.mu.Lock()
		ct, err := l.token.Encrypt(packed)
		if err != nil {
			l.mu.Unlock()
			return nil, err
		}
		raw := m.sendLinkPacket(l, rnswire.PacketData, rnswire.ContextRequest, ct)
		h := rnswire.PacketHash(raw)
		copy(rec.ID[:], h[:HashLen])
		l.pendingRequests()[rec.ID] = rec
		l.mu.Unlock()
	} else {
		h := sha256.Sum256(packed)
		copy(rec.ID[:], h[:HashLen])
		l.mu.Lock()
		l.pendingRequests()[rec.ID] = rec
		l.mu.Unlock()
		if _, err := m.sendResource(l, packed, &rec.ID, false); err != nil {
			l.mu.Lock()
			delete(l.pendingRequests(), rec.ID)
			l.mu.Unlock()
			return nil, err
		}
	}
	go func() {
		t := time.NewTimer(timeout)
		defer t.Stop()
		select {
		case <-t.C:
			m.failRequest(l, rec.ID, ErrRequestTimeout)
		case <-rec.done:
		}
	}()
	return rec, nil
}

// pendingRequests returns the link's outstanding requests; l.mu held.
func (l *Link) pendingRequests() map[[HashLen]byte]*RequestReceipt {
	if l.pending == nil {
		l.pending = map[[HashLen]byte]*RequestReceipt{}
	}
	return l.pending
}

// pendingRequest looks one up without removing it.
func (l *Link) pendingRequest(id [HashLen]byte) *RequestReceipt {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pendingRequests()[id]
}

func (m *LinkManager) takeRequest(l *Link, id [HashLen]byte) *RequestReceipt {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := l.pendingRequests()[id]
	delete(l.pending, id)
	return rec
}

func (m *LinkManager) failRequest(l *Link, id [HashLen]byte, err error) {
	if rec := m.takeRequest(l, id); rec != nil {
		rec.done <- RequestResult{Err: err}
	}
}

// failAllRequests ends every outstanding request on a link that closed.
func (m *LinkManager) failAllRequests(l *Link) {
	l.mu.Lock()
	pending := l.pending
	l.pending = nil
	l.mu.Unlock()
	for _, rec := range pending {
		rec.done <- RequestResult{Err: errors.New("rns: link closed")}
	}
}

// handleRequestPacket is the REQUEST context on a link we answer for.
func (m *LinkManager) handleRequestPacket(l *Link, pkt *Packet) {
	plain, err := l.Decrypt(pkt.Hdr.Data)
	if err != nil {
		return
	}
	m.handleRequest(l, pkt.TruncatedHash(), plain)
}

// handleRequest unpacks a request (from a packet or a resource) and answers
// it through the local destination's handler.
func (m *LinkManager) handleRequest(l *Link, requestID [HashLen]byte, packed []byte) {
	if l.local == nil || l.State() != LinkActive {
		return
	}
	v, _, err := msgpack.UnpackValue(packed)
	if err != nil || v.Kind != msgpack.KindArray || len(v.Array) != 3 || len(v.Array[1].Bin) != HashLen {
		return
	}
	var ph [HashLen]byte
	copy(ph[:], v.Array[1].Bin)
	entry := l.local.requestHandler(ph)
	if entry == nil {
		return
	}
	req := &Request{Path: entry.path, PathHash: ph, ID: requestID, Data: v.Array[2], Link: l}
	req.RequestedAt, _ = v.Array[0].AsFloat()
	if pub := l.RemoteIdentity(); pub != nil {
		if id, err := rnswire.IdentityFromPublicBytes(pub); err == nil {
			req.RemoteIdentity = id
		}
	}
	resp, ok := entry.fn(req)
	if !ok {
		return
	}
	packedResp := msgpack.Pack(msgpack.Array(msgpack.Bin(requestID[:]), resp))
	if len(packedResp) <= l.MDU() {
		l.mu.Lock()
		if ct, err := l.token.Encrypt(packedResp); err == nil {
			m.sendLinkPacket(l, rnswire.PacketData, rnswire.ContextResponse, ct)
		}
		l.mu.Unlock()
		return
	}
	_, _ = m.sendResource(l, packedResp, &requestID, true)
}

// handleResponsePacket is the RESPONSE context on a link we initiated.
func (m *LinkManager) handleResponsePacket(l *Link, pkt *Packet) {
	plain, err := l.Decrypt(pkt.Hdr.Data)
	if err != nil {
		return
	}
	m.handleResponse(l, plain)
}

// handleResponse unpacks [request_id, data] and resolves the receipt.
func (m *LinkManager) handleResponse(l *Link, packed []byte) {
	v, _, err := msgpack.UnpackValue(packed)
	if err != nil || v.Kind != msgpack.KindArray || len(v.Array) != 2 || len(v.Array[0].Bin) != HashLen {
		return
	}
	var id [HashLen]byte
	copy(id[:], v.Array[0].Bin)
	if rec := m.takeRequest(l, id); rec != nil {
		rec.done <- RequestResult{Response: v.Array[1]}
	}
}
