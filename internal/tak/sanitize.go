package tak

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Sanitize is the one door a Cursor-on-Target event from outside the Hub comes
// through before the Hub writes it anywhere (MESHSAT-1458).
//
// # Why the Hub re-writes the event instead of passing the bytes on
//
// A kit or an app exports CoT to the Hub, and the Hub writes it into a TAK
// server's stream and hands it to the tenant's other clients. Two things follow.
//
// The stream to a TAK server is nothing but events laid end to end. Whatever a
// client sends is spliced into it, so a payload holding two events, half an
// event, or a newline and some more markup would be read by the server as
// traffic the Hub vouched for. And what the Hub parses is not what the next
// parser parses: Go's decoder skips a DOCTYPE, hides CDATA and tolerates things
// that a server splitting on "</event>" or a phone's DOM parser will act on.
//
// Re-marshalling through CotEvent would settle both, and lose the event: that
// struct knows seven detail elements, and chat (__chat, link), group, takv,
// colour and every other element a TAK client needs would silently vanish.
//
// So this walks the tokens, checks them, and writes each one back out with the
// Hub's own writer. Every element and attribute the sender wrote survives, in
// order; nothing else does. The result is one line holding exactly one event,
// produced here.
//
// # What is refused
//
//   - anything that is not exactly one <event> element, with nothing after it;
//   - DOCTYPE, entities, comments, CDATA and processing instructions (one leading
//     <?xml?> declaration is tolerated and dropped), and any encoding but UTF-8;
//   - namespace prefixes and xmlns attributes: CoT has none, and a prefix the Hub
//     passes on undeclared is a parse error in the next reader;
//   - an element named event, or starting with it, inside the event: a reader
//     that frames by searching for that name would split there;
//   - an event without uid, type, time, start, stale and one point with a real
//     latitude and longitude; direct children other than point and detail;
//   - an event that went stale more than staleSkew ago;
//   - anything over the Limits it is called with.
//
// A refusal is a *Refused carrying a short Reason, which is safe to use as a
// metric label, and a Detail for a log line.

// Limits bounds one event. The zero value refuses everything.
type Limits struct {
	MaxBytes    int // the payload as received
	MaxDepth    int // element nesting, the event itself being 1
	MaxElements int
	MaxAttrs    int // per element
	MaxAttrLen  int // one attribute value, decoded
}

// ClientLimits is for an event a kit or an app exports. Their events are a few
// hundred bytes; this leaves room for a chat message and nothing like a file.
var ClientLimits = Limits{MaxBytes: 16 << 10, MaxDepth: 12, MaxElements: 512, MaxAttrs: 32, MaxAttrLen: 4 << 10}

// UpstreamLimits is for an event read from a tenant's TAK server, where ATAK's
// own traffic (routes, shapes with many vertices) is larger.
var UpstreamLimits = Limits{MaxBytes: 64 << 10, MaxDepth: 16, MaxElements: 2048, MaxAttrs: 48, MaxAttrLen: 8 << 10}

// staleSkew is how far past its stale time an event may be and still be taken.
// Five minutes covers a sender's clock being off; an event older than that is
// history, and writing it now would draw a past position as a present one.
const staleSkew = 5 * time.Minute

// Reasons a payload is refused. Fixed strings: they are metric label values.
const (
	ReasonSize      = "size"
	ReasonEncoding  = "encoding"
	ReasonMarkup    = "markup"
	ReasonStructure = "structure"
	ReasonNamespace = "namespace"
	ReasonLimits    = "limits"
	ReasonFields    = "fields"
	ReasonStale     = "stale"
)

// Refused is why a payload was not accepted.
type Refused struct {
	Reason string
	Detail string
}

func (r *Refused) Error() string { return "tak: CoT refused (" + r.Reason + "): " + r.Detail }

func refuse(reason, format string, args ...any) error {
	return &Refused{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Meta is what the caller needs to know about an accepted event without parsing
// it again.
type Meta struct {
	UID   string
	Type  string
	Time  time.Time
	Stale time.Time
	Lat   float64
	Lon   float64
	// Emergency is true for an alarm type (b-a...) or an event carrying an
	// <emergency> detail. The caller uses it so an SOS is never queued behind,
	// or rate-limited with, routine position updates.
	Emergency bool
}

// Sanitize checks raw and returns it re-written as one line. See the comment at
// the top of this file.
func Sanitize(raw []byte, lim Limits, now time.Time) ([]byte, Meta, error) {
	var meta Meta
	if len(raw) == 0 {
		return nil, meta, refuse(ReasonSize, "empty payload")
	}
	if len(raw) > lim.MaxBytes {
		return nil, meta, refuse(ReasonSize, "%d bytes, over the %d limit", len(raw), lim.MaxBytes)
	}
	if !utf8.Valid(raw) {
		return nil, meta, refuse(ReasonEncoding, "not valid UTF-8")
	}
	if err := scanMarkup(raw); err != nil {
		return nil, meta, err
	}

	d := xml.NewDecoder(bytes.NewReader(raw))
	d.Strict = true
	// d.Entity stays nil, so only the five predefined entities and numeric
	// references resolve. d.CharsetReader stays nil, so a declared encoding other
	// than UTF-8 is an error rather than something to transcode.

	var (
		out       bytes.Buffer
		stack     []string
		elements  int
		open      bool // a start tag is written and its '>' is not
		rootDone  bool
		sawPoint  bool
		sawDetail bool
		root      map[string]string
	)
	out.Grow(len(raw) + 64)
	closeOpen := func() {
		if open {
			out.WriteByte('>')
			open = false
		}
	}

	for {
		// RawToken, not Token: Token rewrites namespace prefixes into URLs and
		// checks tag matching itself, and this needs the names as written and does
		// its own matching below.
		tok, err := d.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			reason := ReasonStructure
			if strings.Contains(err.Error(), "encoding") {
				reason = ReasonEncoding
			}
			return nil, meta, refuse(reason, "%v", err)
		}

		switch t := tok.(type) {
		case xml.ProcInst:
			// scanMarkup allowed at most one, leading, named xml. It is dropped:
			// the output is a fragment of a stream, not a document.
			continue

		case xml.Directive, xml.Comment:
			// Unreachable after scanMarkup; kept so that removing the scan does
			// not quietly start passing these.
			return nil, meta, refuse(ReasonMarkup, "a directive or comment")

		case xml.CharData:
			if allSpace(t) {
				// Indentation. Dropped everywhere, which is also what keeps the
				// output on one line.
				continue
			}
			switch {
			case len(stack) == 0:
				return nil, meta, refuse(ReasonStructure, "text outside the event")
			case len(stack) == 1:
				return nil, meta, refuse(ReasonStructure, "text directly inside the event")
			case stack[1] == "point":
				return nil, meta, refuse(ReasonStructure, "text inside the point")
			}
			closeOpen()
			if err := xml.EscapeText(&out, t); err != nil {
				return nil, meta, refuse(ReasonStructure, "%v", err)
			}

		case xml.StartElement:
			if rootDone {
				return nil, meta, refuse(ReasonStructure, "a second element after the event")
			}
			if t.Name.Space != "" {
				return nil, meta, refuse(ReasonNamespace, "element %s:%s carries a prefix", t.Name.Space, t.Name.Local)
			}
			name := t.Name.Local
			if !validName(name) {
				return nil, meta, refuse(ReasonStructure, "element name %q", name)
			}
			depth := len(stack) + 1
			if depth > lim.MaxDepth {
				return nil, meta, refuse(ReasonLimits, "nested deeper than %d", lim.MaxDepth)
			}
			if elements++; elements > lim.MaxElements {
				return nil, meta, refuse(ReasonLimits, "more than %d elements", lim.MaxElements)
			}
			if len(t.Attr) > lim.MaxAttrs {
				return nil, meta, refuse(ReasonLimits, "%s has %d attributes, over %d", name, len(t.Attr), lim.MaxAttrs)
			}
			switch {
			case depth == 1:
				if name != "event" {
					return nil, meta, refuse(ReasonStructure, "the root element is %q, not event", name)
				}
			case strings.HasPrefix(name, "event"):
				return nil, meta, refuse(ReasonStructure, "an element named %q inside the event", name)
			case depth == 2 && name == "point":
				if sawPoint {
					return nil, meta, refuse(ReasonStructure, "a second point")
				}
				sawPoint = true
			case depth == 2 && name == "detail":
				if sawDetail {
					return nil, meta, refuse(ReasonStructure, "a second detail")
				}
				sawDetail = true
			case depth == 2:
				return nil, meta, refuse(ReasonStructure, "%q directly inside the event; only point and detail belong there", name)
			case stack[1] == "point":
				return nil, meta, refuse(ReasonStructure, "an element inside the point")
			}
			if depth >= 3 && name == "emergency" && stack[1] == "detail" {
				meta.Emergency = true
			}

			closeOpen()
			out.WriteByte('<')
			out.WriteString(name)
			attrs := make(map[string]string, len(t.Attr))
			for _, a := range t.Attr {
				if a.Name.Space != "" || a.Name.Local == "xmlns" {
					return nil, meta, refuse(ReasonNamespace, "attribute %s:%s on %s", a.Name.Space, a.Name.Local, name)
				}
				if !validName(a.Name.Local) {
					return nil, meta, refuse(ReasonStructure, "attribute name %q on %s", a.Name.Local, name)
				}
				if _, dup := attrs[a.Name.Local]; dup {
					return nil, meta, refuse(ReasonStructure, "attribute %s given twice on %s", a.Name.Local, name)
				}
				if len(a.Value) > lim.MaxAttrLen {
					return nil, meta, refuse(ReasonLimits, "attribute %s on %s is %d bytes, over %d", a.Name.Local, name, len(a.Value), lim.MaxAttrLen)
				}
				attrs[a.Name.Local] = a.Value
				out.WriteByte(' ')
				out.WriteString(a.Name.Local)
				out.WriteString(`="`)
				if err := xml.EscapeText(&out, []byte(a.Value)); err != nil {
					return nil, meta, refuse(ReasonStructure, "%v", err)
				}
				out.WriteByte('"')
			}
			open = true
			stack = append(stack, name)

			switch {
			case depth == 1:
				root = attrs
			case depth == 2 && name == "point":
				lat, lon, err := pointOf(attrs)
				if err != nil {
					return nil, meta, err
				}
				meta.Lat, meta.Lon = lat, lon
			}

		case xml.EndElement:
			if len(stack) == 0 || t.Name.Space != "" || stack[len(stack)-1] != t.Name.Local {
				return nil, meta, refuse(ReasonStructure, "</%s> does not close what is open", t.Name.Local)
			}
			if open {
				out.WriteString("/>")
				open = false
			} else {
				out.WriteString("</")
				out.WriteString(t.Name.Local)
				out.WriteByte('>')
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				rootDone = true
			}
		}
	}

	if !rootDone {
		return nil, meta, refuse(ReasonStructure, "the event is not closed")
	}
	if !sawPoint {
		return nil, meta, refuse(ReasonFields, "no point")
	}
	if err := eventOf(root, &meta, now); err != nil {
		return nil, meta, err
	}
	// Escaping can lengthen a payload. Bounded, so a small input cannot be made
	// into a large write.
	if out.Len() > 2*lim.MaxBytes {
		return nil, meta, refuse(ReasonSize, "%d bytes once written, over twice the %d limit", out.Len(), lim.MaxBytes)
	}
	return out.Bytes(), meta, nil
}

// scanMarkup refuses the markup a CoT event has no use for, by looking at the
// bytes rather than at tokens.
//
// By bytes on purpose. In well-formed XML a '<' never appears in text or in an
// attribute value, so "<!" and "<?" can only be markup: a DOCTYPE, an entity
// declaration, a comment, a CDATA section, a processing instruction. The decoder
// would report most of those as tokens, but it HIDES CDATA -- it hands back the
// contents as ordinary text -- and that is the one a reader downstream is most
// likely to treat differently. A byte search cannot be talked out of seeing it.
func scanMarkup(raw []byte) error {
	b := bytes.TrimLeft(raw, " \t\r\n")
	if bytes.HasPrefix(b, []byte("<?xml")) && len(b) > 5 && (b[5] == ' ' || b[5] == '\t' || b[5] == '?' || b[5] == '\r' || b[5] == '\n') {
		end := bytes.Index(b, []byte("?>"))
		if end < 0 {
			return refuse(ReasonMarkup, "an XML declaration that does not end")
		}
		b = b[end+2:]
	}
	if bytes.Contains(b, []byte("<!")) {
		return refuse(ReasonMarkup, "a DOCTYPE, comment, CDATA section or declaration")
	}
	if bytes.Contains(b, []byte("<?")) {
		return refuse(ReasonMarkup, "a processing instruction")
	}
	return nil
}

// eventOf checks the event's own attributes and fills meta from them.
func eventOf(a map[string]string, meta *Meta, now time.Time) error {
	uid := a["uid"]
	if uid == "" || len(uid) > 255 {
		return refuse(ReasonFields, "uid is empty or over 255 bytes")
	}
	for _, r := range uid {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return refuse(ReasonFields, "uid carries a control character")
		}
	}
	typ := a["type"]
	if typ == "" || len(typ) > 64 {
		return refuse(ReasonFields, "type is empty or over 64 bytes")
	}
	for i := 0; i < len(typ); i++ {
		switch c := typ[i]; {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return refuse(ReasonFields, "type %q is not a CoT type", typ)
		}
	}
	var times [3]time.Time
	for i, k := range []string{"time", "start", "stale"} {
		t, err := time.Parse(time.RFC3339Nano, a[k])
		if err != nil {
			return refuse(ReasonFields, "%s %q is not a timestamp", k, a[k])
		}
		times[i] = t
	}
	if times[2].Before(now.Add(-staleSkew)) {
		return refuse(ReasonStale, "went stale at %s", times[2].UTC().Format(time.RFC3339))
	}
	meta.UID, meta.Type = uid, typ
	meta.Time, meta.Stale = times[0], times[2]
	if strings.HasPrefix(typ, TypeAlarm) {
		meta.Emergency = true
	}
	return nil
}

// pointOf checks the point's coordinates.
func pointOf(a map[string]string) (lat, lon float64, err error) {
	num := func(k string, required bool) (float64, error) {
		v, ok := a[k]
		if !ok {
			if required {
				return 0, refuse(ReasonFields, "the point has no %s", k)
			}
			return 0, nil
		}
		f, perr := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if perr != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, refuse(ReasonFields, "the point's %s %q is not a number", k, v)
		}
		return f, nil
	}
	if lat, err = num("lat", true); err != nil {
		return 0, 0, err
	}
	if lon, err = num("lon", true); err != nil {
		return 0, 0, err
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, refuse(ReasonFields, "the point %v,%v is not on the Earth", lat, lon)
	}
	for _, k := range []string{"hae", "ce", "le"} {
		if _, err = num(k, false); err != nil {
			return 0, 0, err
		}
	}
	return lat, lon, nil
}

// validName accepts the names CoT uses: a letter or underscore, then letters,
// digits, underscore, dot and hyphen. No colon: prefixes are refused before this.
func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case i > 0 && (c >= '0' && c <= '9' || c == '.' || c == '-'):
		default:
			return false
		}
	}
	return true
}

func allSpace(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			return false
		}
	}
	return true
}
