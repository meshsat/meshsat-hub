package mail

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeRelay is a one-shot SMTP server that records the DATA it was given.
func fakeRelay(t *testing.T) (addr string, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		r := bufio.NewReader(c)
		w := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		w("220 fake ESMTP")
		var body strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					got <- body.String()
					w("250 Ok: queued")
					inData = false
					continue
				}
				body.WriteString(line + "\n")
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				w("250-fake")
				w("250 SIZE 10240000")
			case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
				w("250 Ok")
			case line == "DATA":
				w("354 End data with <CR><LF>.<CR><LF>")
				inData = true
			case line == "QUIT":
				w("221 Bye")
				return
			default:
				w("250 Ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestSendProducesAWellFormedMessage(t *testing.T) {
	addr, got := fakeRelay(t)
	s := New(addr, "billing@meshsat.net", "MeshSat Hub", 5*time.Second)
	if s == nil {
		t.Fatal("New returned nil for a configured relay")
	}
	if err := s.Send(context.Background(), "alice@example.com", "Your plan", "Hello Alice,\n\nBody line.\n"); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case msg := <-got:
		for _, want := range []string{
			`From: "MeshSat Hub" <billing@meshsat.net>`,
			"To: <alice@example.com>",
			"Subject: Your plan",
			"Content-Type: text/plain; charset=utf-8",
			"Auto-Submitted: auto-generated",
			"Hello Alice,",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("message missing %q\n---\n%s", want, msg)
			}
		}
		// A message with no Message-ID scores as spam at most receivers, and
		// one added by the relay would fall outside the DKIM signature.
		if !strings.Contains(msg, "Message-ID: <") || !strings.Contains(msg, "@meshsat.net>") {
			t.Errorf("no Message-ID in the sender's own domain:\n%s", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay never received the message")
	}
}

// A newline in a header would let a display name or a plan label add its own
// Bcc. Neither field is free text today, but both carry user-shaped data.
func TestHeaderInjectionIsRefused(t *testing.T) {
	addr, _ := fakeRelay(t)
	s := New(addr, "billing@meshsat.net", "MeshSat Hub", 2*time.Second)
	for _, tc := range []struct{ name, to, subject string }{
		{"subject", "alice@example.com", "Hi\r\nBcc: attacker@example.com"},
		{"recipient", "alice@example.com\r\nRCPT TO:<attacker@example.com>", "Hi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Send(context.Background(), tc.to, tc.subject, "body"); err == nil {
				t.Fatal("a header with a newline was accepted")
			}
		})
	}
}

// A line that is just "." would end DATA early and truncate the message.
func TestBodyLinesAreDotStuffed(t *testing.T) {
	addr, got := fakeRelay(t)
	s := New(addr, "billing@meshsat.net", "MeshSat Hub", 5*time.Second)
	if err := s.Send(context.Background(), "a@b.example", "s", "before\n.\nafter"); err != nil {
		t.Fatalf("send: %v", err)
	}
	msg := <-got
	if !strings.Contains(msg, "before") || !strings.Contains(msg, "after") {
		t.Fatalf("body was truncated at the lone dot:\n%s", msg)
	}
}

func TestUnconfiguredIsNilAndSafe(t *testing.T) {
	if New("", "billing@meshsat.net", "MeshSat Hub", 0) != nil {
		t.Error("New returned a sender with no relay")
	}
	if New("host:25", "", "MeshSat Hub", 0) != nil {
		t.Error("New returned a sender with no from address")
	}
	var s *SMTP
	if err := s.Send(context.Background(), "a@b.example", "s", "b"); err != ErrNotConfigured {
		t.Errorf("nil sender: %v, want ErrNotConfigured", err)
	}
	// SendOrLog must swallow it: no message here is worth failing a payment,
	// an approval or a lapse over.
	SendOrLog(context.Background(), nil, "a@b.example", "s", "b", "test")
}

func TestGreetingHandlesWhatItIsGiven(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "Hello,",
		"Alice Example":     "Hello Alice,",
		"alice@example.com": "Hello,",
		"  Bob  ":           "Hello Bob,",
	} {
		if got := greeting(in); got != want {
			t.Errorf("greeting(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLapseWarningCountsDownAndCarriesTheClaimCode(t *testing.T) {
	subject, body := LapseWarning("Alice", "crew", time.Now().Add(72*time.Hour), "https://ko-fi.com/x", "AB2K9XYZ")
	if !strings.Contains(subject, "crew") {
		t.Errorf("subject does not name the plan: %q", subject)
	}
	for _, want := range []string{"AB2K9XYZ", "https://ko-fi.com/x", "an SOS is never affected"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// The reassurance matters: a lapse changes the ceiling, not the service.
	if !strings.Contains(body, "keeps working") {
		t.Errorf("body does not say existing kit keeps working:\n%s", body)
	}
}
