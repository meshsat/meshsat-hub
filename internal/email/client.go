package email

import (
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
)

// Client sends emails via SMTP with optional PGP encryption.
type Client struct {
	host     string // SMTP host:port
	from     string // sender email address
	username string // SMTP auth username (often same as from)
	password string // SMTP auth password
	keyRing  *KeyRing
}

// NewClient creates a new SMTP email client.
func NewClient(host, from, username, password string, kr *KeyRing) *Client {
	return &Client{
		host:     host,
		from:     from,
		username: username,
		password: password,
		keyRing:  kr,
	}
}

// Send sends an email. If the recipient has a PGP key in the keyring,
// the body is PGP-encrypted. Otherwise, sends in cleartext.
func (c *Client) Send(to, subject, body string) error {
	if to == "" {
		return fmt.Errorf("email: empty recipient")
	}

	// Attempt PGP encryption if keyring available.
	encrypted := false
	if c.keyRing != nil {
		encBody, wasEncrypted, err := c.keyRing.Encrypt(to, body)
		if err != nil {
			slog.Warn("email: PGP encryption failed, sending cleartext", "to", to, "error", err)
		} else if wasEncrypted {
			body = encBody
			encrypted = true
		}
	}

	// Build RFC 2822 message.
	var msg strings.Builder
	msg.WriteString("From: " + c.from + "\r\n")
	msg.WriteString("To: " + to + "\r\n")
	msg.WriteString("Subject: " + sanitizeHeader(subject) + "\r\n")
	msg.WriteString("MIME-Version: 1.0\r\n")
	if encrypted {
		msg.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		msg.WriteString("X-MeshSat-PGP: encrypted\r\n")
	} else {
		msg.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	}
	msg.WriteString("\r\n")
	msg.WriteString(body)

	// Send via SMTP.
	host, _, err := net.SplitHostPort(c.host)
	if err != nil {
		host = c.host
	}

	var auth smtp.Auth
	if c.username != "" {
		auth = smtp.PlainAuth("", c.username, c.password, host)
	}

	if err := smtp.SendMail(c.host, auth, c.from, []string{to}, []byte(msg.String())); err != nil {
		return fmt.Errorf("email: smtp send: %w", err)
	}

	slog.Info("email: sent", "to", to, "subject", subject, "encrypted", encrypted)
	return nil
}

// sanitizeHeader removes newlines to prevent header injection.
func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}
