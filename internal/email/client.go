package email

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/netguard"
)

// Client sends emails via SMTP with optional PGP encryption.
type Client struct {
	host     string // SMTP host:port
	from     string // sender email address
	username string // SMTP auth username (often same as from)
	password string // SMTP auth password
	keyRing  *KeyRing
	// guard dials through netguard: a TENANT'S server is an address a
	// customer typed, and without the guard it could name the database or the
	// broker inside the cluster. The operator's own relay is internal by
	// design and is not guarded.
	guard   bool
	timeout time.Duration
}

// NewClient creates the operator's SMTP client (HUB_EMAIL_*).
func NewClient(host, from, username, password string, kr *KeyRing) *Client {
	return &Client{
		host:     host,
		from:     from,
		username: username,
		password: password,
		keyRing:  kr,
		timeout:  30 * time.Second,
	}
}

// NewTenantClient creates the client for a tenant's own mail server, dialled
// only to a public address (netguard.DialControl, after resolution).
func NewTenantClient(host, from, username, password string, kr *KeyRing) *Client {
	c := NewClient(host, from, username, password, kr)
	c.guard = true
	return c
}

// From is the address the client sends as.
func (c *Client) From() string { return c.from }

// Message is one mail.
type Message struct {
	To      string
	Subject string
	Body    string
	// ReplyTo, when set, is where the recipient's mail client sends a reply.
	ReplyTo string
	// InReplyTo and References thread the mail on an earlier one: each a
	// Message-ID with its angle brackets, References space-separated.
	InReplyTo  string
	References string
	// AsIs sends Body exactly as given and never encrypts it here: an
	// end-to-end body is already ciphertext for the recipient, and a second
	// layer to the recipient's key would only make it unreadable to anyone
	// who does not hold both.
	AsIs bool
	// RequireTLS refuses to hand the mail to a server that offers no TLS.
	// The "@" lane's mail carries the conversation's reference, which is
	// what authorises a reply to reach a device (MESHSAT-1685).
	RequireTLS bool
}

// Send sends an email. If the recipient has a PGP key in the keyring,
// the body is PGP-encrypted. Otherwise, sends in cleartext.
func (c *Client) Send(to, subject, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	_, err := c.SendMessage(ctx, Message{To: to, Subject: subject, Body: body})
	return err
}

// SendMessage sends one mail and returns the Message-ID it carries, so a
// reply can be matched to it.
func (c *Client) SendMessage(ctx context.Context, m Message) (string, error) {
	to := strings.TrimSpace(m.To)
	if to == "" {
		return "", fmt.Errorf("email: empty recipient")
	}
	// Every header value is refused rather than cleaned when it carries a line
	// break: a device label, a contact name or a subject is user-shaped data,
	// and one CRLF in it is a Bcc header of the sender's choosing.
	for _, v := range []string{to, m.Subject, m.ReplyTo, m.InReplyTo, m.References} {
		if strings.ContainsAny(v, "\r\n") {
			return "", errors.New("email: line break in a header")
		}
	}
	if a, err := mail.ParseAddress(to); err != nil || a.Address != to {
		return "", fmt.Errorf("email: %q is not one bare address", to)
	}
	if m.ReplyTo != "" {
		if _, err := mail.ParseAddress(m.ReplyTo); err != nil {
			return "", fmt.Errorf("email: reply-to: %w", err)
		}
	}

	body := m.Body
	encrypted := false
	if !m.AsIs && c.keyRing != nil {
		encBody, wasEncrypted, err := c.keyRing.Encrypt(to, body)
		if err != nil {
			slog.Warn("email: PGP encryption failed, sending cleartext", "to", to, "error", err)
		} else if wasEncrypted {
			body = encBody
			encrypted = true
		}
	}
	// Cleartext is a legitimate outcome -- most recipients have no PGP key -- but
	// it was entirely SILENT, which is how contacts being wiped by a rollout or
	// living on one replica of two produced no signal at all (MESHSAT-1123). The
	// alert still goes out; it is now possible to notice that it went out in the
	// clear.
	if !encrypted && !m.AsIs {
		slog.Warn("email: no PGP key on file for this recipient, sending CLEARTEXT", "to", to)
	}

	id := messageID(c.from)
	raw := c.render(to, m, id, body, encrypted)
	if err := c.deliver(ctx, to, raw, m.RequireTLS); err != nil {
		return "", err
	}
	slog.Info("email: sent", "to", to, "encrypted", encrypted, "as_is", m.AsIs)
	return id, nil
}

// render builds the message. Date and Message-ID were missing before
// MESHSAT-1685, and a mail without them scores as spam at most receivers or
// is refused outright; RFC 5322 requires Date.
func (c *Client) render(to string, m Message, id, body string, encrypted bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", c.from)
	fmt.Fprintf(&b, "To: <%s>\r\n", to)
	if m.ReplyTo != "" {
		fmt.Fprintf(&b, "Reply-To: %s\r\n", m.ReplyTo)
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", encodeHeader(sanitizeHeader(m.Subject)))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: %s\r\n", id)
	if m.InReplyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\n", m.InReplyTo)
	}
	if m.References != "" {
		fmt.Fprintf(&b, "References: %s\r\n", m.References)
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	if encrypted {
		b.WriteString("X-MeshSat-PGP: encrypted\r\n")
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	if needsQP(body) {
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		var qp strings.Builder
		w := quotedprintable.NewWriter(&qp)
		_, _ = w.Write([]byte(body))
		_ = w.Close()
		body = qp.String()
	} else {
		b.WriteString("Content-Transfer-Encoding: 7bit\r\n\r\n")
	}
	// Line endings only. net/smtp's DATA writer is a textproto.DotWriter and
	// dot-stuffs on the way out; stuffing here as well is how a line of "."
	// arrived as ".." on the transactional path (internal/mail).
	for line := range strings.SplitSeq(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	return b.String()
}

// deliver runs the SMTP conversation: implicit TLS on 465, STARTTLS where the
// server offers it, and credentials only over TLS.
func (c *Client) deliver(ctx context.Context, to, raw string, requireTLS bool) error {
	host, port, err := net.SplitHostPort(c.host)
	if err != nil {
		host, port = c.host, "25"
	}
	addr := net.JoinHostPort(host, port)
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	d := &net.Dialer{Timeout: 15 * time.Second}
	if c.guard {
		d.Control = netguard.DialControl
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("email: dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	tlsConf := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	implicitTLS := port == "465"
	if implicitTLS {
		tc := tls.Client(conn, tlsConf)
		if err := tc.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("email: TLS to %s: %w", addr, err)
		}
		conn = tc
	}
	s, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("email: greeting: %w", err)
	}
	defer func() { _ = s.Quit() }()
	// Go says "localhost" unless told otherwise, and a server that requires a
	// fully-qualified HELO refuses the whole conversation (the same defect the
	// transactional path had, internal/mail).
	if err := s.Hello(heloFor(c.from)); err != nil {
		return fmt.Errorf("email: EHLO: %w", err)
	}
	secure := implicitTLS
	if !secure {
		if ok, _ := s.Extension("STARTTLS"); ok {
			if err := s.StartTLS(tlsConf); err != nil {
				return fmt.Errorf("email: STARTTLS: %w", err)
			}
			secure = true
		}
	}
	if requireTLS && !secure {
		return fmt.Errorf("email: %s offers no TLS; this mail is not sent in the clear", addr)
	}
	if c.username != "" {
		// A password never crosses the wire in the clear. smtp.PlainAuth
		// refuses that too, except to localhost; saying why here is kinder
		// than its error.
		if !secure {
			return fmt.Errorf("email: %s offers no TLS; refusing to send the SMTP password in the clear", addr)
		}
		if err := s.Auth(smtp.PlainAuth("", c.username, c.password, host)); err != nil {
			return fmt.Errorf("email: auth: %w", err)
		}
	}
	if err := s.Mail(c.from); err != nil {
		return fmt.Errorf("email: MAIL FROM: %w", err)
	}
	if err := s.Rcpt(to); err != nil {
		return fmt.Errorf("email: RCPT TO: %w", err)
	}
	w, err := s.Data()
	if err != nil {
		return fmt.Errorf("email: DATA: %w", err)
	}
	if _, err := w.Write([]byte(raw)); err != nil {
		return fmt.Errorf("email: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email: smtp send: %w", err)
	}
	return nil
}

// heloFor derives a fully-qualified HELO name from the sender address.
func heloFor(from string) string {
	if i := strings.LastIndexByte(from, '@'); i >= 0 && i+1 < len(from) && strings.Contains(from[i+1:], ".") {
		return from[i+1:]
	}
	return "meshsat.net"
}

// messageID builds a unique id in the sender's own domain, angle brackets
// included.
func messageID(from string) string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return "<" + hex.EncodeToString(raw[:]) + ".hub@" + heloFor(from) + ">"
}

// encodeHeader puts a non-ASCII subject in RFC 2047 form; a raw UTF-8 header
// is mangled or refused by part of the mail world.
func encodeHeader(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}

// needsQP: non-ASCII, or a line longer than SMTP's 998-octet limit.
func needsQP(body string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		if len(line) > 900 {
			return true
		}
	}
	for i := 0; i < len(body); i++ {
		if body[i] >= 0x80 {
			return true
		}
	}
	return false
}

// sanitizeHeader removes newlines to prevent header injection.
func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}
