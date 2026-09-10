// Package mail sends the Hub's transactional email: the few messages a customer
// must receive because something happened to their account.
//
// It talks to an IP-authorised relay with no credentials, which is how Invoice
// Ninja submits from the same estate. The relay signs outbound mail with DKIM
// for meshsat.net and is the only hop that reaches the internet, so nothing here
// needs TLS, authentication or a queue of its own.
//
// Deliberately not a general mailer. There is no HTML, no template engine and
// no retry: these messages are short, plain text, and a failure is logged
// rather than retried, because none of them is worth wedging the caller. A
// receipt is not sent from here -- Invoice Ninja sends it, from its own outbox.
//
// The one exception is the credit note, which travels as a PDF attached to the
// plain-text notice that explains it (SendWith). Invoice Ninja can email a
// credit note itself, but in 5.13.31 that path goes through a mailer with no
// per-company sender, so a MeshSat customer would receive it under the other
// company's identity. Sending it from here is the fix, and it keeps the
// document and the words about it in one message rather than two.
package mail

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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

// Attachment is a file that travels with a message.
type Attachment struct {
	// Filename is what the recipient sees. It reaches a mail header, so it is
	// sanitised before use rather than trusted.
	Filename string
	// ContentType is the MIME type, e.g. "application/pdf".
	ContentType string
	Content     []byte
}

// AttachmentSender delivers a message carrying one file. It is a separate
// interface from Sender so that the many callers who never attach anything are
// not forced to grow a method, and so a test double can implement one without
// the other.
type AttachmentSender interface {
	SendWith(ctx context.Context, to, subject, body string, att Attachment) error
}

// ErrNotConfigured means no relay address was supplied, so nothing is sent.
var ErrNotConfigured = errors.New("mail: no relay configured")

// SMTP is a Sender that submits to a relay over plain SMTP.
type SMTP struct {
	Addr     string // host:port of the relay
	From     string // envelope and header From
	FromName string // display name
	Timeout  time.Duration
	// HELO is the name this client announces itself by. It must be a
	// fully-qualified domain name: Go's net/smtp defaults to "localhost", and a
	// relay with reject_non_fqdn_helo_hostname refuses that -- which is exactly
	// what happened here. Hub replicas reach the relay from a different tunnel
	// address per worker node, one of which happened to be allowlisted, so
	// customer mail was delivered from one pod and rejected from the others
	// with nothing but a 504 in the relay's log to say so (MESHSAT-1019).
	HELO string
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
	return &SMTP{Addr: addr, From: from, FromName: fromName, Timeout: timeout, HELO: heloFor(from)}
}

// heloFor derives a fully-qualified HELO name from the sender address, so a
// Hub that configures nothing still announces something a relay will accept.
func heloFor(from string) string {
	if i := strings.LastIndexByte(from, '@'); i >= 0 && i+1 < len(from) && strings.Contains(from[i+1:], ".") {
		return from[i+1:]
	}
	return "meshsat.net"
}

// Send delivers one message. The context bounds the whole conversation.
func (s *SMTP) Send(ctx context.Context, to, subject, body string) error {
	return s.deliver(ctx, to, subject, func() string { return s.message(to, subject, body) })
}

// SendWith delivers one message with a file attached.
func (s *SMTP) SendWith(ctx context.Context, to, subject, body string, att Attachment) error {
	return s.deliver(ctx, to, subject, func() string { return s.messageWith(to, subject, body, att) })
}

func (s *SMTP) deliver(ctx context.Context, to, subject string, render func() string) error {
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

	// Announce a real name. Without this Go says "localhost" and a relay that
	// requires a fully-qualified HELO rejects the whole conversation.
	helo := s.HELO
	if helo == "" {
		helo = heloFor(s.From)
	}
	if err := c.Hello(helo); err != nil {
		return fmt.Errorf("mail: EHLO %s: %w", helo, err)
	}

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
	if _, err := w.Write([]byte(render())); err != nil {
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

// headers builds everything above the body. Shared so a plain message and one
// with an attachment cannot drift apart in the parts that decide whether a
// receiver treats the message as spam.
func (s *SMTP) headers(to, subject string) *strings.Builder {
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
	return &b
}

// writeBody writes the plain-text body with dot-stuffing: a line that is just
// "." would end DATA early.
func writeBody(b *strings.Builder, body string) {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, ".") {
			line = "." + line
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
}

// messageWith renders a multipart/mixed message: the text the person reads and
// one file. Used for the credit note, which has to travel with the words that
// explain it.
func (s *SMTP) messageWith(to, subject, body string, att Attachment) string {
	boundary := "meshsat-" + strings.TrimSuffix(s.messageID(), "@"+domainOf(s.From))
	b := s.headers(to, subject)
	fmt.Fprintf(b, "Content-Type: multipart/mixed; boundary=%q\r\n", boundary)
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("\r\n")

	fmt.Fprintf(b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	writeBody(b, body)

	ct := att.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	fmt.Fprintf(b, "\r\n--%s\r\n", boundary)
	fmt.Fprintf(b, "Content-Type: %s\r\n", ct)
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	fmt.Fprintf(b, "Content-Disposition: attachment; filename=%q\r\n\r\n", safeFilename(att.Filename))
	// Base64 in 76-character lines: longer ones are legal in theory and
	// mangled in practice.
	enc := base64.StdEncoding.EncodeToString(att.Content)
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")
		enc = enc[76:]
	}
	if enc != "" {
		b.WriteString(enc)
		b.WriteString("\r\n")
	}
	fmt.Fprintf(b, "--%s--\r\n", boundary)
	return b.String()
}

// safeFilename keeps a filename inside a header. Nothing user-supplied reaches
// this today, but a quoted header field that can carry a quote or a newline is
// a header injection waiting for its first caller.
func safeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '"', r == '\\', r < 0x20, r == 0x7f:
			return -1
		}
		return r
	}, name)
	if name == "" {
		return "attachment"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 && i+1 < len(addr) {
		return addr[i+1:]
	}
	return "meshsat.net"
}

func (s *SMTP) message(to, subject, body string) string {
	b := s.headers(to, subject)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("\r\n")
	writeBody(b, body)
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
