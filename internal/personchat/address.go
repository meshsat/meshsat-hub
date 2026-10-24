// Package personchat is the "@" lane: a tenant's device writes to a PERSON by
// SMS or email through the Hub, and the person's reply comes back to that
// device (MESHSAT-1685).
//
//	device -> person: a text from the tenant's own satellite modem, or from a
//	kit's SIM, that starts with an address, "@+31612345678 water at the gate"
//	or "@ops@example.org water at the gate", goes to that number by the
//	tenant's Twilio account or to that address by the tenant's mail gateway.
//
//	person -> device: the person answers the SMS or the email, and the Hub
//	sends the answer to the device that wrote to them, as a satellite MT to its
//	modem or an SMS to the kit's SIM, "[+31612345678] on my way".
//
// Every message is plain or end to end, the sender's choice. End to end means
// the Hub reads the address and nothing else: the body is a sealed MeshSat SMS
// frame (for a recipient running MeshSat Android) or an OpenPGP message, and
// the Hub carries it unaltered both ways (e2e.go).
//
// The first character picks the lane, as it does elsewhere: "*" is the
// satellite chat lane (internal/satchat), "#" the visitor SMS chat
// (internal/booth), "@" this one.
package personchat

import (
	"net/mail"
	"strings"
	"unicode"
)

// Kind is how the Hub reaches a person.
type Kind string

const (
	KindSMS   Kind = "sms"
	KindEmail Kind = "email"
	// KindName is a contact of the tenant's directory, named by its display
	// name; it resolves to one of the two above (Resolve).
	KindName Kind = "name"
)

// Target is the person a text is addressed to, as written.
type Target struct {
	Kind  Kind
	Value string // E.164 with "+", a lower-cased address, or the name as written
}

// maxEmailLen is RFC 5321's limit on a forward path.
const maxEmailLen = 254

// ParseAddressed splits "@<address> <body>" into the target and the body. The
// address is the first token after a leading "@", ended by whitespace or a
// colon; the body is the rest and must not be empty.
//
// What it accepts is narrow on purpose. A text that starts with "@" is often
// just a mention on a mesh ("@bob are you there"), and the lane must not turn
// that into a paid SMS or an email to a stranger, so a name is only a target
// once the directory knows it (the caller resolves it), and a number or an
// address must be exactly one.
func ParseAddressed(text string) (Target, string, bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "@") {
		return Target{}, "", false
	}
	t = t[1:]
	end := strings.IndexFunc(t, func(r rune) bool { return unicode.IsSpace(r) || r == ':' })
	if end <= 0 {
		return Target{}, "", false
	}
	token, rest := t[:end], t[end:]
	body := strings.TrimSpace(strings.TrimPrefix(strings.TrimLeftFunc(rest, unicode.IsSpace), ":"))
	if body == "" {
		return Target{}, "", false
	}
	switch {
	case strings.HasPrefix(token, "+"):
		n, ok := E164(token)
		if !ok {
			return Target{}, "", false
		}
		return Target{Kind: KindSMS, Value: n}, body, true
	case strings.Contains(token, "@"):
		a, ok := EmailAddress(token)
		if !ok {
			return Target{}, "", false
		}
		return Target{Kind: KindEmail, Value: a}, body, true
	default:
		if !validName(token) {
			return Target{}, "", false
		}
		return Target{Kind: KindName, Value: token}, body, true
	}
}

// E164 reports whether s is a phone number in E.164 form: "+", a non-zero
// digit, then 6 to 14 more digits. Nothing is normalised: a number the sender
// wrote with spaces or a trunk prefix is not one the Hub can be sure of.
func E164(s string) (string, bool) {
	if len(s) < 8 || len(s) > 16 || s[0] != '+' || s[1] < '1' || s[1] > '9' {
		return "", false
	}
	for i := 2; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "", false
		}
	}
	return s, true
}

// EmailAddress reports whether s is one bare mail address, and returns it
// with the domain lower-cased. A display name, a comment, quoting, a route or
// anything that is not printable ASCII is refused: the address goes into a
// header, and the reply that comes back is matched against it byte for byte.
func EmailAddress(s string) (string, bool) {
	if s == "" || len(s) > maxEmailLen || strings.Count(s, "@") != 1 {
		return "", false
	}
	for _, r := range s {
		if r <= ' ' || r > '~' || strings.ContainsRune(`"(),:;<>[\]`, r) {
			return "", false
		}
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s {
		return "", false
	}
	at := strings.LastIndexByte(s, '@')
	local, domain := s[:at], strings.ToLower(s[at+1:])
	if local == "" || !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", false
	}
	return local + "@" + domain, true
}

// NormaliseEmail is the form an inbound From is compared in: the bare address
// out of "Name <a@b>", domain lower-cased. The local part keeps its case, as
// RFC 5321 lets a server treat it; the lane wrote to exactly that spelling.
func NormaliseEmail(from string) (string, bool) {
	a, err := mail.ParseAddress(strings.TrimSpace(from))
	if err != nil {
		return EmailAddress(strings.TrimSpace(from))
	}
	return EmailAddress(a.Address)
}

// validName is what may name a directory contact in a text: letters, digits
// and . _ - only, at most 64. A contact's display name is compared with its
// spaces removed (NameKey).
func validName(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// NameKey is how a contact's display name is matched by a written name:
// spaces removed, case folded. "Ops Desk" is written "@OpsDesk" or "@opsdesk".
func NameKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), ""))
}
