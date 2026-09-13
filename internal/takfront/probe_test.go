package takfront

import (
	"crypto/tls"
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
		// Kept as errors.New ON PURPOSE. This asserts that peerVanished does not
		// demote it to the probe path; whether it is a handshake refusal or
		// plaintext is spokePlaintext's decision, tested below against the real
		// tls.RecordHeaderError value rather than a string that looks like one.
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

// MESHSAT-1074, the second half. Plaintext on the TLS port is separated from a
// handshake refusal, because on a port open to the whole internet the two need
// different answers and used to be one label.
//
// These build the REAL tls.RecordHeaderError rather than an errors.New that reads
// like one. That distinction is the whole point: a string-matching predicate would
// pass either way, and a type-matching one passes only against what crypto/tls
// actually returns.
func TestPlaintextOnTheTLSPortIsItsOwnThing(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = server.Close() })

	// Exactly what crypto/tls builds at conn.go's "first record does not look
	// like a TLS handshake": Conn is set ONLY for this case.
	realPlaintext := tls.RecordHeaderError{
		Msg:          "first record does not look like a TLS handshake",
		RecordHeader: [5]byte{'G', 'E', 'T', ' ', '/'},
		Conn:         server,
	}
	if !spokePlaintext(realPlaintext) {
		t.Error("an HTTP request to the TAK port was not recognised as plaintext, " +
			"so it stays a WARN and buries the refusals worth reading")
	}
	if !spokePlaintext(fmt.Errorf("handshake: %w", realPlaintext)) {
		t.Error("wrapping hid it; errors.As must unwrap to the value")
	}

	// The other three RecordHeaderError constructions leave Conn nil, and every
	// one of them is a middlebox or protocol fault worth a WARN. Matching on the
	// type alone would swallow all three.
	for _, tc := range []struct {
		what string
		msg  string
	}{
		{"an SSLv2 hello", "unsupported SSLv2 handshake received"},
		{"a version mismatch mid-stream", "received record with version 0303 when expecting version 0301"},
		{"an oversized record", "oversized record received with length 66666"},
	} {
		if spokePlaintext(tls.RecordHeaderError{Msg: tc.msg}) {
			t.Errorf("%s: demoted to Info, but it is a protocol fault and must stay a WARN", tc.what)
		}
	}

	// And the classifier must not steal anything from the other two paths.
	for _, tc := range []struct {
		what string
		err  error
	}{
		{"a vanished peer", io.EOF},
		{"an unknown issuer", errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority")},
		{"no certificate offered", errors.New("tls: client didn't provide a certificate")},
		{"a stalled handshake", &net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}},
		{"nothing at all", nil},
		// Pins TYPE matching over text matching. This string is what the real
		// error prints, so a strings.Contains implementation would classify it
		// and be wrong: anything at all can carry that text, including a wrapped
		// error from somewhere else entirely.
		{"a plain error that merely reads like one",
			errors.New("tls: first record does not look like a TLS handshake")},
	} {
		if spokePlaintext(tc.err) {
			t.Errorf("%s: classified as plaintext, which would hide a real refusal at Info", tc.what)
		}
	}
	if peerVanished(realPlaintext) {
		t.Error("plaintext counted as a vanished peer, which would hide it at Debug entirely")
	}
}

// The end-to-end shape, against a real TLS server rather than a constructed
// error: write an HTTP request at a TLS listener and read back what the handshake
// actually produces. This is what proves the predicate matches production and not
// just our idea of it.
func TestARealHTTPRequestToTheTLSPortIsPlaintext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	got := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- err
			return
		}
		// defer, not t.Cleanup: this is a goroutine, and registering a cleanup
		// from one races the test finishing and panics if it loses.
		defer func() { _ = c.Close() }()
		got <- tls.Server(c, &tls.Config{Certificates: []tls.Certificate{}}).Handshake()
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Write([]byte("GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	err = <-got
	if err == nil {
		t.Fatal("the handshake succeeded against a plaintext client")
	}
	if !spokePlaintext(err) {
		t.Fatalf("the error a real TLS server returns for a real HTTP request is not "+
			"classified as plaintext: %T %v", err, err)
	}
	if peerVanished(err) {
		t.Error("it would be hidden at Debug")
	}
}
