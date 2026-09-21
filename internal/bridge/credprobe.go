package bridge

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sort"
	"sync"
	"time"
)

// A provisioning bundle used to be claimable before the broker would accept
// its credentials (MESHSAT-1298). Generating one stores a new bcrypt hash and
// re-renders the NATS users file at once, but the file reaches each member
// through the kubelet's sync of the mounted Secret and the reloader's SIGHUP:
// 27 to 59 s later on the three members, measured 21 Sep 2026. The phone that
// scanned the QR connected 8 s after it was generated with the right password
// and was refused ("authentication error - User msa-flaneur").
//
// CredentialProber answers the question the Hub could not: does every broker
// member accept this user and password yet? It logs in, over the in-cluster
// MQTT listener, exactly as the bridge will, and logs straight out again.

// ProbeResult is one broker member's answer.
type ProbeResult struct {
	Addr     string `json:"addr"`
	Accepted bool   `json:"accepted"`
	Detail   string `json:"detail,omitempty"`
}

// CredentialProber checks a credential against every member behind HostPort,
// a headless Service ("nats-headless.<ns>.svc.cluster.local:1883") whose name
// resolves to one address per member.
type CredentialProber struct {
	HostPort string
	Timeout  time.Duration

	lookup func(ctx context.Context, host string) ([]string, error)
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)

	mu    sync.Mutex
	cache map[string]cachedProbe
}

type cachedProbe struct {
	at      time.Time
	results []ProbeResult
}

// probeCacheFor bounds how often one credential is tried: the claim endpoint
// is polled, and every login costs each member a bcrypt comparison.
//
// Only a LIVE answer is cached. Each replica has its own cache, and the Fleet
// page's status poll and the phone's claim land on either one: caching "2 of
// 3" let a claim on one pod answer 503 up to two seconds after the other pod
// had shown "Scan now" (seen live, 21 Sep 2026). A member that accepts a
// credential keeps accepting it, so a positive answer cannot go stale that way.
const probeCacheFor = 2 * time.Second

// NewCredentialProber returns a prober for hostPort ("host:port").
func NewCredentialProber(hostPort string) *CredentialProber {
	d := &net.Dialer{}
	return &CredentialProber{HostPort: hostPort, Timeout: 3 * time.Second,
		lookup: net.DefaultResolver.LookupHost, dial: d.DialContext, cache: map[string]cachedProbe{}}
}

// Live reports whether every member accepts user/password, with each member's
// answer. No member found is not live.
func (p *CredentialProber) Live(ctx context.Context, user, pass string) (bool, []ProbeResult, error) {
	res, err := p.Check(ctx, user, pass)
	if err != nil {
		return false, res, err
	}
	for _, r := range res {
		if !r.Accepted {
			return false, res, nil
		}
	}
	return len(res) > 0, res, nil
}

// Check logs in to every member once and returns each answer.
func (p *CredentialProber) Check(ctx context.Context, user, pass string) ([]ProbeResult, error) {
	key := user + "\x00" + pass
	p.mu.Lock()
	if c, ok := p.cache[key]; ok && time.Since(c.at) < probeCacheFor {
		p.mu.Unlock()
		return c.results, nil
	}
	p.mu.Unlock()

	host, port, err := net.SplitHostPort(p.HostPort)
	if err != nil {
		return nil, fmt.Errorf("credprobe: %q is not host:port: %w", p.HostPort, err)
	}
	addrs, err := p.lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("credprobe: resolve %s: %w", host, err)
	}
	sort.Strings(addrs)
	results := make([]ProbeResult, len(addrs))
	var wg sync.WaitGroup
	for i, a := range addrs {
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			results[i] = p.one(ctx, addr, user, pass)
		}(i, net.JoinHostPort(a, port))
	}
	wg.Wait()

	p.mu.Lock()
	for k, c := range p.cache { // keep the map small
		if time.Since(c.at) > time.Minute {
			delete(p.cache, k)
		}
	}
	if allAccepted(results) {
		p.cache[key] = cachedProbe{at: time.Now(), results: results}
	}
	p.mu.Unlock()
	return results, nil
}

func allAccepted(res []ProbeResult) bool {
	for _, r := range res {
		if !r.Accepted {
			return false
		}
	}
	return len(res) > 0
}

func (p *CredentialProber) one(ctx context.Context, addr, user, pass string) ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	conn, err := p.dial(ctx, "tcp", addr)
	if err != nil {
		return ProbeResult{Addr: addr, Detail: "unreachable: " + err.Error()}
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	rc, err := mqttLogin(conn, probeClientID(), user, pass)
	if err != nil {
		return ProbeResult{Addr: addr, Detail: err.Error()}
	}
	if rc != 0 {
		return ProbeResult{Addr: addr, Detail: connackText(rc)}
	}
	_, _ = conn.Write([]byte{0xE0, 0x00}) // DISCONNECT
	return ProbeResult{Addr: addr, Accepted: true}
}

// probeClientID is never the bridge's own id: a second session under the
// bridge's client id would take its live connection over.
func probeClientID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "hub-credprobe-" + hex.EncodeToString(b)
}

// mqttLogin sends an MQTT 3.1.1 CONNECT with a clean session and returns the
// CONNACK return code (0 accepted, 4 bad user name or password, 5 not
// authorised).
func mqttLogin(rw io.ReadWriter, clientID, user, pass string) (byte, error) {
	var body []byte
	for i, f := range []string{"MQTT", clientID, user, pass} {
		enc, err := mqttString(f)
		if err != nil {
			return 0, err
		}
		body = append(body, enc...)
		if i == 0 {
			body = append(body, 0x04, 0xC2, 0x00, 0x0A) // level 4; user name, password, clean session; keep-alive 10 s
		}
	}
	pkt := []byte{0x10}
	for n := len(body); ; {
		b := byte(n % 128)
		n /= 128
		if n > 0 {
			b |= 0x80
		}
		pkt = append(pkt, b)
		if n == 0 {
			break
		}
	}
	pkt = append(pkt, body...)
	if _, err := rw.Write(pkt); err != nil {
		return 0, fmt.Errorf("connect: %w", err)
	}
	var ack [4]byte
	if _, err := io.ReadFull(rw, ack[:]); err != nil {
		// NATS closes the socket on a refused login rather than always sending
		// a CONNACK with a code, so a close here is a refusal.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 5, nil
		}
		return 0, fmt.Errorf("connack: %w", err)
	}
	if ack[0] != 0x20 || ack[1] != 0x02 {
		return 0, fmt.Errorf("connack: unexpected %x", ack[:2])
	}
	return ack[3], nil
}

// mqttString is an MQTT UTF-8 string: a two-byte big-endian length, then the
// bytes, at most 65535 of them.
func mqttString(s string) ([]byte, error) {
	n := len(s)
	if n < 0 || n > math.MaxUint16 {
		return nil, errors.New("credprobe: a field is longer than an MQTT string can be")
	}
	b := make([]byte, 2, 2+n)
	binary.BigEndian.PutUint16(b, uint16(n))
	return append(b, s...), nil
}

func connackText(rc byte) string {
	switch rc {
	case 4:
		return "refused: bad user name or password"
	case 5:
		return "refused: not authorised"
	}
	return fmt.Sprintf("refused: connack code %d", rc)
}
