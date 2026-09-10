// Package mail sends the Hub's transactional email: the few messages a customer
// must receive because something happened to their account.
//
// It talks to an IP-authorised relay with no credentials, which is how Invoice
// Ninja submits from the same estate. The relay signs outbound mail with DKIM
// for meshsat.net and is the only hop that reaches the internet, so nothing here
// needs TLS, authentication or a queue of its own.
//
// Deliberately not a general mailer. There is no template engine and no retry:
// these messages are short, there are six of them, and a failure is logged
// rather than retried, because none of them is worth wedging the caller. A
// receipt is not sent from here -- Invoice Ninja sends it, from its own outbox.
//
// Every message is built twice, as text and as HTML, and goes out as
// multipart/alternative. The HTML is the billing system's own shell (html.go),
// because a customer receives a Hub notice and an Invoice Ninja receipt about
// the same payment minutes apart and two visual identities for one company is
// a reason to distrust both. The text part is not a stub: it carries every
// fact the HTML does.
//
// The credit note additionally travels as a PDF (SendMessageWith). Invoice
// Ninja can email a credit note itself, but in 5.13.31 that path goes through a
// mailer with no per-company sender, so a MeshSat customer would receive it
// under the other company's identity. Sending it from here is the fix, and it
// keeps the document and the words about it in one message rather than two.
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

// Message is one message to a customer, in both the shapes a mail client may
// want it. Text is mandatory; HTML is optional and, when present, the message
// goes out as multipart/alternative so a plain-text reader still gets a
// readable version rather than a wall of markup.
type Message struct {
	Subject string
	Text    string
	HTML    string
}

// Sender delivers a message. Split out so callers can be tested without a relay.
type Sender interface {
	SendMessage(ctx context.Context, to string, m Message) error
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
	SendMessageWith(ctx context.Context, to string, m Message, att Attachment) error
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

// SendMessage delivers one message. The context bounds the whole conversation.
func (s *SMTP) SendMessage(ctx context.Context, to string, m Message) error {
	return s.deliver(ctx, to, m.Subject, func() string { return s.render(to, m, nil) })
}

// SendMessageWith delivers one message with a file attached.
func (s *SMTP) SendMessageWith(ctx context.Context, to string, m Message, att Attachment) error {
	return s.deliver(ctx, to, m.Subject, func() string { return s.render(to, m, &att) })
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

// writeBody writes a plain-text part, normalising line endings to CRLF.
//
// It deliberately does NOT dot-stuff. net/smtp's DATA writer is a
// textproto.DotWriter, which stuffs on the way out; doing it here as well put
// two layers on the wire, of which a receiver strips one, so a body line of "."
// arrived as "..". Every part of the message goes through the same writer, so
// the QP and base64 parts are covered by the same reasoning.
func writeBody(b *strings.Builder, body string) {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		b.WriteString(line)
		b.WriteString("\r\n")
	}
}

// textCTE is the encoding header the plain part needs, if any.
//
// A customer's name is whatever their identity provider holds, and half of
// Europe's are not ASCII. text/plain with no Content-Transfer-Encoding means
// 7bit, so an unencoded "Jos\u00e9" is 8-bit octets in a part that claims to have
// none -- undefined at any hop that is not 8BITMIME, and this Hub does not
// choose its relay's capabilities. Encode when there is something to encode,
// and leave the ordinary all-ASCII message readable in a raw spool file.
func textCTE(text string) string {
	if isASCII(text) {
		return ""
	}
	return "Content-Transfer-Encoding: quoted-printable\r\n"
}

func writeTextBody(b *strings.Builder, text string) {
	if isASCII(text) {
		writeBody(b, text)
		return
	}
	writeQuotedPrintable(b, text)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// render builds the whole MIME message.
//
// Four shapes, and the nesting matters. multipart/alternative says "the same
// content twice, pick one"; multipart/mixed says "these parts are all part of
// the message". A credit note is both -- two renderings of the words plus a
// document -- so the alternative goes INSIDE the mixed, which is the only
// arrangement that shows a reader the HTML and the PDF rather than making them
// choose between them.
func (s *SMTP) render(to string, m Message, att *Attachment) string {
	b := s.headers(to, m.Subject)

	switch {
	case m.HTML == "" && att == nil:
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		b.WriteString(textCTE(m.Text))
		b.WriteString(autoSubmitted)
		b.WriteString("\r\n")
		writeTextBody(b, m.Text)

	case m.HTML != "" && att == nil:
		alt := s.boundary("alt")
		fmt.Fprintf(b, "Content-Type: multipart/alternative; boundary=%q\r\n", alt)
		b.WriteString(autoSubmitted)
		b.WriteString("\r\n")
		writeAlternative(b, alt, m)
		fmt.Fprintf(b, "--%s--\r\n", alt)

	default:
		mix := s.boundary("mix")
		fmt.Fprintf(b, "Content-Type: multipart/mixed; boundary=%q\r\n", mix)
		b.WriteString(autoSubmitted)
		b.WriteString("\r\n")
		fmt.Fprintf(b, "--%s\r\n", mix)
		if m.HTML == "" {
			b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
			b.WriteString(textCTE(m.Text))
			b.WriteString("\r\n")
			writeTextBody(b, m.Text)
		} else {
			alt := s.boundary("alt")
			fmt.Fprintf(b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", alt)
			writeAlternative(b, alt, m)
			fmt.Fprintf(b, "--%s--\r\n", alt)
		}
		writeAttachment(b, mix, *att)
		fmt.Fprintf(b, "--%s--\r\n", mix)
	}
	return b.String()
}

const autoSubmitted = "Auto-Submitted: auto-generated\r\n"

// boundary returns a delimiter that cannot occur in the content it separates.
func (s *SMTP) boundary(kind string) string {
	return "meshsat-" + kind + "-" + strings.TrimSuffix(s.messageID(), "@"+domainOf(s.From))
}

// writeAlternative writes the plain part then the HTML part. Order is not
// cosmetic: RFC 2046 says the LAST alternative is the richest, and clients
// pick accordingly.
func writeAlternative(b *strings.Builder, boundary string, m Message) {
	fmt.Fprintf(b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString(textCTE(m.Text))
	b.WriteString("\r\n")
	writeTextBody(b, m.Text)
	fmt.Fprintf(b, "\r\n--%s\r\n", boundary)
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	writeQuotedPrintable(b, m.HTML)
	b.WriteString("\r\n")
}

func writeAttachment(b *strings.Builder, boundary string, att Attachment) {
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
}

// writeQuotedPrintable encodes the HTML part.
//
// The shell carries a 32 KB base64 logo on one line, and SMTP has a 998-octet
// line limit that a raw 8-bit send would blow straight through -- the message
// would be mangled or refused. quoted-printable also keeps the markup roughly
// readable in a raw dump, which matters when the next person is reading a spool
// file to work out whether something was signed.
func writeQuotedPrintable(b *strings.Builder, s string) {
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		writeQPLine(b, line)
	}
}

// writeQPLine encodes one source line, inserting soft breaks so no output line
// runs past 76 characters.
func writeQPLine(b *strings.Builder, line string) {
	col := 0
	emit := func(tok string) {
		// Never split an escape across the break, and leave room for the "="
		// that marks the break itself.
		if col+len(tok) > 75 {
			b.WriteString("=\r\n")
			col = 0
		}
		b.WriteString(tok)
		col += len(tok)
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == ' ' || c == '\t':
			// Whitespace at the end of a line must be encoded. A receiver is
			// entitled to strip trailing whitespace before a line break, and
			// would do it silently, so the encoding is what preserves it.
			if i == len(line)-1 {
				emit(fmt.Sprintf("=%02X", c))
			} else {
				emit(string(c))
			}
		case c >= 33 && c <= 126 && c != '=':
			emit(string(c))
		default:
			// Everything else, which covers UTF-8 byte by byte.
			emit(fmt.Sprintf("=%02X", c))
		}
	}
	b.WriteString("\r\n")
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

// SendOrLog sends and swallows the error into a log line. Every caller in the
// Hub uses this: none of these messages is worth failing an approval, a payment
// or a lapse over, and all of them are recoverable by a person.
func SendOrLog(ctx context.Context, s Sender, to string, m Message, what string) {
	if s == nil {
		slog.Debug("mail: no relay configured, not sending", "what", what, "to", to)
		return
	}
	if err := s.SendMessage(ctx, to, m); err != nil {
		slog.Error("mail: could not send", "what", what, "to", to, "error", err)
		return
	}
	slog.Info("mail: sent", "what", what, "to", to, "html", m.HTML != "")
}
