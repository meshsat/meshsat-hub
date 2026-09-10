// Package mail sends the Hub's transactional email: the few messages a customer
// must receive because something happened to their account.
//
// It talks to an IP-authorised relay with no credentials, which is how Invoice
// Ninja submits from the same estate. The relay signs outbound mail with DKIM
// for meshsat.net and is the only hop that reaches the internet, so nothing here
// needs TLS, authentication or a queue of its own.
//
// Deliberately not a general mailer. There is no HTML, no attachment, no
// template engine and no retry: these messages are short, plain text, and a
// failure is logged rather than retried, because none of them is worth wedging
// the caller. A receipt is the one piece of customer mail that must not be lost,
// and it is not sent from here -- Invoice Ninja sends it, from its own outbox.
package mail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Sender delivers a message. Split out so callers can be tested without a relay.
type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
}

// ErrNotConfigured means no relay address was supplied, so nothing is sent.
var ErrNotConfigured = errors.New("mail: no relay configured")

// SMTP is a Sender that submits to a relay over plain SMTP.
type SMTP struct {
	Addr     string // host:port of the relay
	From     string // envelope and header From
	FromName string // display name
	Timeout  time.Duration
}

// New returns a Sender, or nil if addr is empty. A nil *SMTP is safe to call:
// it reports ErrNotConfigured rather than panicking, so a Hub without a relay
// configured simply does not send.
func New(addr, from, fromName string, timeout time.Duration) *SMTP {
	if strings.TrimSpace(addr) == "" || strings.TrimSpace(from) == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &SMTP{Addr: addr, From: from, FromName: fromName, Timeout: timeout}
}

// Send delivers one message. The context bounds the whole conversation.
func (s *SMTP) Send(ctx context.Context, to, subject, body string) error {
	if s == nil {
		return ErrNotConfigured
	}
	to = strings.TrimSpace(to)
	if to == "" {
		return errors.New("mail: no recipient")
	}
	// A header injected through the subject would let a display name or a plan
	// label add its own Bcc. Neither field is free text today, but they pass
	// through user-shaped data, so refuse rather than rely on that.
	if strings.ContainsAny(subject, "\r\n") || strings.ContainsAny(to, "\r\n") {
		return errors.New("mail: newline in a header")
	}

	deadline := time.Now().Add(s.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("mail: dial %s: %w", s.Addr, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)

	host, _, _ := net.SplitHostPort(s.Addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("mail: greeting: %w", err)
	}
	defer func() { _ = c.Quit() }()

	if err := c.Mail(s.From); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write([]byte(s.message(to, subject, body))); err != nil {
		return fmt.Errorf("mail: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: close: %w", err)
	}
	return nil
}

// messageID builds a unique id in the sender's own domain.
func (s *SMTP) messageID() string {
	domain := "meshsat.net"
	if i := strings.LastIndexByte(s.From, '@'); i >= 0 && i+1 < len(s.From) {
		domain = s.From[i+1:]
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%d.hub@%s", time.Now().UnixNano(), domain)
	}
	return fmt.Sprintf("%s.hub@%s", hex.EncodeToString(raw[:]), domain)
}

func (s *SMTP) message(to, subject, body string) string {
	from := s.From
	if s.FromName != "" {
		from = fmt.Sprintf("%q <%s>", s.FromName, s.From)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: <%s>\r\n", to)
	fmt.Fprintf(&b, "Reply-To: %s\r\n", from)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	// A message with no Message-ID scores as spam at most receivers -- a bare
	// test message without one landed at spam score 4 where a receipt with one
	// scores 0. Postfix would synthesise one, but only after the milter has
	// signed, so it would sit outside the DKIM signature.
	fmt.Fprintf(&b, "Message-ID: <%s>\r\n", s.messageID())
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("\r\n")
	// Dot-stuffing: a line that is just "." would end DATA early.
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, ".") {
			line = "." + line
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	return b.String()
}

// SendOrLog sends and swallows the error into a log line. Every caller in the
// Hub uses this: none of these messages is worth failing an approval, a payment
// or a lapse over, and all of them are recoverable by a person.
func SendOrLog(ctx context.Context, s Sender, to, subject, body, what string) {
	if s == nil {
		slog.Debug("mail: no relay configured, not sending", "what", what, "to", to)
		return
	}
	if err := s.Send(ctx, to, subject, body); err != nil {
		slog.Error("mail: could not send", "what", what, "to", to, "error", err)
		return
	}
	slog.Info("mail: sent", "what", what, "to", to)
}
