package directory

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// vCard 4.0 (RFC 6350) import / export with X-MESHSAT-* extensions
// for the bearer kinds standard vCard does not cover (MESHTASTIC,
// APRS, IRIDIUM_SBD, IRIDIUM_IMT, CELLULAR, TAK, RETICULUM, ZIGBEE,
// BLE, WEBHOOK, MQTT) plus TEAM / ROLE / SIDC / TRUST-LEVEL. This
// mirrors the bridge-side implementation byte-for-byte on the wire
// so directory .vcf files round-trip between Hub and Bridge without
// transformation. [MESHSAT-541]

var vcardXMeshsatKinds = map[string]AddressKind{
	"X-MESHSAT-MESHTASTIC":  KindMeshtastic,
	"X-MESHSAT-APRS":        KindAPRS,
	"X-MESHSAT-IRIDIUM-SBD": KindIridiumSBD,
	"X-MESHSAT-IRIDIUM-IMT": KindIridiumIMT,
	"X-MESHSAT-CELLULAR":    KindCellular,
	"X-MESHSAT-TAK":         KindTAK,
	"X-MESHSAT-RETICULUM":   KindReticulum,
	"X-MESHSAT-ZIGBEE":      KindZigbee,
	"X-MESHSAT-BLE":         KindBLE,
	"X-MESHSAT-WEBHOOK":     KindWebhook,
	"X-MESHSAT-MQTT":        KindMQTT,
}

// ParseVCards reads BEGIN:VCARD..END:VCARD blocks from r and returns
// the corresponding Contact records. Unrecognised properties are
// silently skipped. Returns the first syntactic error encountered.
func ParseVCards(r io.Reader) ([]Contact, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		contacts []Contact
		current  *Contact
		inCard   bool
	)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		switch {
		case strings.EqualFold(trimmed, "BEGIN:VCARD"):
			if inCard {
				return nil, fmt.Errorf("line %d: nested BEGIN:VCARD", lineNo)
			}
			inCard = true
			current = &Contact{Origin: OriginImported}
		case strings.EqualFold(trimmed, "END:VCARD"):
			if !inCard {
				return nil, fmt.Errorf("line %d: END:VCARD without BEGIN", lineNo)
			}
			if current.DisplayName == "" && (current.GivenName != "" || current.FamilyName != "") {
				current.DisplayName = strings.TrimSpace(current.GivenName + " " + current.FamilyName)
			}
			if current.DisplayName == "" {
				return nil, fmt.Errorf("line %d: vCard has no FN or N", lineNo)
			}
			contacts = append(contacts, *current)
			current = nil
			inCard = false
		default:
			if !inCard {
				continue
			}
			if err := applyVCardLine(current, line); err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("vcard scan: %w", err)
	}
	if inCard {
		return nil, fmt.Errorf("unterminated VCARD block")
	}
	return contacts, nil
}

func applyVCardLine(c *Contact, line string) error {
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return nil
	}
	head := line[:colon]
	value := unescapeVCardValue(line[colon+1:])

	nameParts := strings.Split(head, ";")
	name := strings.ToUpper(strings.TrimSpace(nameParts[0]))
	params := map[string]string{}
	for _, p := range nameParts[1:] {
		if eq := strings.IndexByte(p, '='); eq > 0 {
			params[strings.ToUpper(p[:eq])] = strings.TrimSpace(p[eq+1:])
		}
	}

	switch name {
	case "VERSION":
	case "FN":
		c.DisplayName = value
	case "N":
		nfields := strings.SplitN(value, ";", 5)
		if len(nfields) > 0 {
			c.FamilyName = nfields[0]
		}
		if len(nfields) > 1 {
			c.GivenName = nfields[1]
		}
	case "ORG":
		c.Org = strings.SplitN(value, ";", 2)[0]
	case "TITLE":
		if c.Role == "" {
			c.Role = value
		}
	case "NOTE":
		c.Notes = value
	case "UID":
		if c.ID == "" {
			c.ID = value
		}
	case "TEL":
		_ = params // reserved; TYPE=voice/cell/text all map to SMS
		appendAddress(c, KindSMS, value)
	case "EMAIL":
		appendAddress(c, KindEmail, value)
	case "X-MESHSAT-TEAM":
		c.Team = value
	case "X-MESHSAT-ROLE":
		c.Role = value
	case "X-MESHSAT-SIDC":
		c.SIDC = value
	case "X-MESHSAT-TRUST-LEVEL":
		if n, err := strconv.Atoi(value); err == nil && n >= 0 && n <= 3 {
			c.TrustLevel = TrustLevel(n)
		}
	default:
		if kind, ok := vcardXMeshsatKinds[name]; ok {
			appendAddress(c, kind, value)
		}
	}
	return nil
}

func appendAddress(c *Contact, kind AddressKind, value string) {
	if !kind.Valid() || value == "" {
		return
	}
	c.Addresses = append(c.Addresses, Address{
		Kind:        kind,
		Value:       value,
		PrimaryRank: primaryRankFor(c, kind),
	})
}

func primaryRankFor(c *Contact, kind AddressKind) int {
	for _, a := range c.Addresses {
		if a.Kind == kind {
			return 1
		}
	}
	return 0
}

// WriteVCards emits the contacts as vCard 4.0 BEGIN/END blocks
// separated by CRLF.
func WriteVCards(w io.Writer, contacts []Contact) error {
	bw := bufio.NewWriter(w)
	for i := range contacts {
		if err := writeOneVCard(bw, &contacts[i]); err != nil {
			return err
		}
	}
	return bw.Flush()
}

func writeOneVCard(w *bufio.Writer, c *Contact) error {
	var werr error
	put := func(name, value string) {
		if werr != nil || value == "" {
			return
		}
		_, werr = fmt.Fprintf(w, "%s:%s\r\n", name, escapeVCardValue(value))
	}
	if _, err := fmt.Fprintln(w, "BEGIN:VCARD\r"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "VERSION:4.0\r"); err != nil {
		return err
	}
	put("FN", c.DisplayName)
	if c.FamilyName != "" || c.GivenName != "" {
		if _, err := fmt.Fprintf(w, "N:%s;%s;;;\r\n",
			escapeVCardValue(c.FamilyName), escapeVCardValue(c.GivenName)); err != nil {
			return err
		}
	}
	put("UID", c.ID)
	put("ORG", c.Org)
	put("TITLE", c.Role)
	put("NOTE", c.Notes)
	put("X-MESHSAT-TEAM", c.Team)
	put("X-MESHSAT-ROLE", c.Role)
	put("X-MESHSAT-SIDC", c.SIDC)
	if c.TrustLevel > 0 {
		if _, err := fmt.Fprintf(w, "X-MESHSAT-TRUST-LEVEL:%d\r\n", int(c.TrustLevel)); err != nil {
			return err
		}
	}
	for _, a := range c.Addresses {
		if werr != nil {
			break
		}
		switch a.Kind {
		case KindSMS:
			_, werr = fmt.Fprintf(w, "TEL;TYPE=cell:%s\r\n", escapeVCardValue(a.Value))
		case KindEmail:
			put("EMAIL", a.Value)
		default:
			for key, k := range vcardXMeshsatKinds {
				if k == a.Kind {
					put(key, a.Value)
					break
				}
			}
		}
	}
	if werr != nil {
		return werr
	}
	if _, err := fmt.Fprintln(w, "END:VCARD\r"); err != nil {
		return err
	}
	return nil
}

func escapeVCardValue(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		";", `\;`,
		",", `\,`,
		"\n", `\n`,
		"\r", "",
	)
	return r.Replace(s)
}

func unescapeVCardValue(s string) string {
	b := strings.Builder{}
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n', 'N':
				b.WriteByte('\n')
				i++
				continue
			case ';', ',', '\\':
				b.WriteByte(s[i+1])
				i++
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}
