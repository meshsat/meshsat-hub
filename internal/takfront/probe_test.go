package takfront

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// MESHSAT-1074. The edge relay health-checks the TAK port by opening a TCP
// connection and closing it, so the handshake ends in EOF -- and that was logged
// as "connection refused reason=handshake" at WARN, byte for byte what a rejected
// phone looks like. Three relays against two replicas, continuously, for ever.
//
// The test that matters is the SECOND one: quieting a log is only safe if the
// thing being quieted cannot also be a real refusal.

func TestAVanishedPeerIsNotARefusal(t *testing.T) {
	for _, tc := range []struct {
		what string
		err  error
	}{
		{"the health check closed without a ClientHello", io.EOF},
		{"a truncated ClientHello", io.ErrUnexpectedEOF},
		{"our own listener closed under it", net.ErrClosed},
		{"the peer reset the connection", syscall.ECONNRESET},
		// Real errors arrive wrapped in *net.OpError, which is how the TLS stack
		// hands them up. If unwrapping is what makes this work, say so in a test.
		{"EOF wrapped the way net wraps it",
			&net.OpError{Op: "read", Net: "tcp", Err: io.EOF}},
		{"a reset wrapped through os.SyscallError",
			&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}},
		{"wrapped again by fmt", fmt.Errorf("handshake: %w", io.EOF)},
	} {
		if !peerVanished(tc.err) {
			t.Errorf("%s: treated as a refusal, so it would log WARN on every probe", tc.what)
		}
	}
}

// The other direction, and the one that makes the change safe to ship: a genuine
// refusal must STILL be loud. These are the errors a real phone produces when its
// certificate is wrong, and every one of them has to stay at WARN.
func TestARealRefusalIsStillLoud(t *testing.T) {
	for _, tc := range []struct {
		what string
		err  error
	}{
		{"an unknown issuer", errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority")},
		{"an expired certificate", errors.New("x509: certificate has expired or is not yet valid")},
		{"no certificate offered", errors.New("tls: client didn't provide a certificate")},
		{"a tenant we could not resolve", ErrUnknownIssuer},
		{"a bad record", errors.New("tls: first record does not look like a TLS handshake")},
		// A stall is NOT a vanished peer: a handshake that starts and then hangs
		// can mean a real network fault and is worth a WARN.
		{"a handshake that stalled", &net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}},
	} {
		if peerVanished(tc.err) {
			t.Errorf("%s: would be demoted to Debug, hiding a genuine refusal", tc.what)
		}
	}
	if peerVanished(nil) {
		t.Error("a nil error counted as a vanished peer")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
