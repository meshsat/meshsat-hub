package warnings

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/meshsat/meshsat-hub/internal/compress"
	"github.com/meshsat/meshsat-hub/internal/geo"
)

// Digest v1 (docs/warning-relay.md "Digest v1"): one warning in at most
// MaxDigestBytes, so it fits one Iridium SBD MT (270 bytes) with room, and
// one Meshtastic packet. The same bytes go on the kit's warn/in topic and,
// when the tenant pays for it, as the MT itself: the first byte doubles as
// the satellite MT type byte, so the kit's modem gateway hands it over
// untouched. Every field is big-endian. The last 64 bytes are the signature
// of everything before them, by the Hub's directory key (ECDSA P-256 over
// SHA-256, r and s each 32 bytes), which the kit pins already.
//
//	off len  field
//	 0   1   magic 0x57 'W'
//	 1   1   version 0x01
//	 2   1   msgType in the low 4 bits (1 Alert, 2 Update, 3 Cancel); bit 7 = test or exercise
//	 3   1   source (SourceDWD 1, SourceMeteoAlarmNL 2, SourceTest 255)
//	 4   8   id: the first 8 bytes of SHA-256 of the CAP identifier
//	12   8   ref: the same of the identifier this Update or Cancel replaces; zero for an Alert
//	20   4   sent, Unix seconds
//	24   4   expires, Unix seconds, 0 for none
//	28   1   severity << 4 | urgency
//	29   1   certainty << 4 | category
//	30   1   event code (EventWind ...)
//	31   2   language, ISO 639-1 ASCII ("de", "nl", "en"); 0 0 unknown
//	33   1   area kind << 4 | count (AreaNone 0; AreaGeocodes 1, at most 8; AreaPolygon 2, at most 12 points)
//	34   var area: a geocode is [type 1][value 9 bytes ASCII, zero padded]; a polygon point is [lat int24][lon int24], degrees * 10000
//	var  1   text length n
//	var  n   text, SMAZ2 with the Meshtastic dictionary (the headline, shortened to fit)
//	end  64  signature over bytes [0, end)
const (
	Magic   byte = 0x57
	Version byte = 0x01

	// MaxDigestBytes keeps a digest in one SBD MT with room for the modem.
	MaxDigestBytes = 234
	// SignatureBytes is r || s of a P-256 signature.
	SignatureBytes = 64
	// MaxText is the most text an encoder offers before it shortens.
	MaxText = 150

	headerBytes  = 34
	maxGeocodes  = 8
	maxPolygon   = 12
	geocodeBytes = 10
	pointBytes   = 6
	// MinDigestBytes is a digest with no area and no text.
	MinDigestBytes = headerBytes + 1 + SignatureBytes

	flagTest = 0x80
)

// Message types.
const (
	TypeAlert  = 1
	TypeUpdate = 2
	TypeCancel = 3
)

// Sources.
const (
	SourceDWD          = 1
	SourceMeteoAlarmNL = 2
	SourceTest         = 255
)

// SourceName names a source id as the feed rows and the kits call it.
func SourceName(id int) string {
	switch id {
	case SourceDWD:
		return "dwd"
	case SourceMeteoAlarmNL:
		return "meteoalarm-nl"
	case SourceTest:
		return "test"
	}
	return "unknown"
}

// SourceID is the inverse of SourceName; 0 for an unknown name.
func SourceID(name string) int {
	switch name {
	case "dwd":
		return SourceDWD
	case "meteoalarm-nl":
		return SourceMeteoAlarmNL
	case "test":
		return SourceTest
	}
	return 0
}

// CAP code tables. 0 is always unknown.
var (
	severities  = []string{"Unknown", "Minor", "Moderate", "Severe", "Extreme"}
	urgencies   = []string{"Unknown", "Future", "Expected", "Immediate", "Past"}
	certainties = []string{"Unknown", "Unlikely", "Possible", "Likely", "Observed"}
	categories  = []string{"Other", "Met", "Geo", "Safety", "Security", "Rescue", "Fire", "Health", "Env", "Transport", "Infra", "CBRNE"}
)

// Event codes: a small table over the free-text CAP event, from the feeds'
// own codes (MeteoAlarm awareness_type, DWD GROUP) or the event words.
const (
	EventOther        = 0
	EventWind         = 1
	EventRain         = 2
	EventSnowIce      = 3
	EventThunderstorm = 4
	EventFog          = 5
	EventHeat         = 6
	EventCold         = 7
	EventCoastal      = 8
	EventFlood        = 9
	EventAvalanche    = 10
	EventWildfire     = 11
	EventTornado      = 12
)

var eventNames = []string{"Other", "Wind", "Rain", "Snow/ice", "Thunderstorm", "Fog", "Heat", "Cold", "Coastal", "Flood", "Avalanche", "Wildfire", "Tornado"}

// EventName names an event code.
func EventName(code int) string {
	if code < 0 || code >= len(eventNames) {
		return eventNames[0]
	}
	return eventNames[code]
}

// Area kinds.
const (
	AreaNone     = 0
	AreaGeocodes = 1
	AreaPolygon  = 2
)

// Geocode types in a digest.
const (
	GeocodeOther      = 0
	GeocodeWarnCellID = 1 // DWD WARNCELLID
	GeocodeEMMAID     = 2 // MeteoAlarm EMMA_ID
	GeocodeNUTS       = 3
)

// Geocode is one area code in a digest.
type Geocode struct {
	Type  int
	Value string
}

// Digest is a decoded digest.
type Digest struct {
	MsgType   int
	Test      bool
	Source    int
	ID        [8]byte
	Ref       [8]byte
	Sent      time.Time
	Expires   time.Time // zero for none
	Severity  int
	Urgency   int
	Certainty int
	Category  int
	Event     int
	Language  string
	Geocodes  []Geocode
	Polygon   []geo.Point
	Text      string
}

// IDOf is the 8-byte id a CAP identifier gets in a digest.
func IDOf(identifier string) [8]byte {
	var id [8]byte
	if identifier == "" {
		return id
	}
	sum := sha256.Sum256([]byte(identifier))
	copy(id[:], sum[:8])
	return id
}

// Signer signs a digest. *directory.TrustAnchor is one.
type Signer interface {
	// Sign returns an ASN.1 DER ECDSA signature over SHA-256(msg).
	Sign(msg []byte) ([]byte, error)
	// PublicKey returns the key in PKIX DER.
	PublicKey() []byte
}

var (
	ErrTooLong   = errors.New("warnings: digest over the size limit")
	ErrBadDigest = errors.New("warnings: not a digest")
	ErrSignature = errors.New("warnings: signature does not verify")
)

// Encode writes d and signs it. The text is shortened until the whole fits
// MaxDigestBytes; geocodes past 8 and polygon points past 12 are dropped
// (the polygon is simplified first).
func Encode(d *Digest, signer Signer) ([]byte, error) {
	if signer == nil {
		return nil, errors.New("warnings: no signer")
	}
	body, err := encodeBody(d)
	if err != nil {
		return nil, err
	}
	der, err := signer.Sign(body)
	if err != nil {
		return nil, fmt.Errorf("warnings: sign: %w", err)
	}
	sig, err := rawSignature(der)
	if err != nil {
		return nil, err
	}
	return append(body, sig...), nil
}

// encodeBody writes everything but the signature.
func encodeBody(d *Digest) ([]byte, error) {
	if d.MsgType < TypeAlert || d.MsgType > TypeCancel {
		return nil, fmt.Errorf("warnings: message type %d", d.MsgType)
	}
	if d.Source < 0 || d.Source > 255 {
		return nil, fmt.Errorf("warnings: source %d", d.Source)
	}
	b := make([]byte, 0, MaxDigestBytes)
	b = append(b, Magic, Version)
	tf := byte(d.MsgType & 0x0F)
	if d.Test {
		tf |= flagTest
	}
	b = append(b, tf, byte(d.Source))
	b = append(b, d.ID[:]...)
	b = append(b, d.Ref[:]...)
	b = binary.BigEndian.AppendUint32(b, unix32(d.Sent))
	b = binary.BigEndian.AppendUint32(b, unix32(d.Expires))
	b = append(b, nibbles(d.Severity, d.Urgency), nibbles(d.Certainty, d.Category), clampByte(d.Event))
	lang := [2]byte{}
	if l := strings.ToLower(d.Language); len(l) >= 2 && isASCIILetter(l[0]) && isASCIILetter(l[1]) {
		lang[0], lang[1] = l[0], l[1]
	}
	b = append(b, lang[:]...)

	// The area.
	var area []byte
	kind, count := AreaNone, 0
	switch {
	case len(d.Geocodes) > 0:
		kind = AreaGeocodes
		for _, g := range d.Geocodes {
			if count == maxGeocodes {
				break
			}
			v := g.Value
			if len(v) > 9 || !isASCII(v) {
				continue
			}
			slot := make([]byte, geocodeBytes)
			slot[0] = clampByte(g.Type)
			copy(slot[1:], v)
			area = append(area, slot...)
			count++
		}
	case len(d.Polygon) >= 3:
		kind = AreaPolygon
		pts := fitPolygon(d.Polygon, maxPolygon)
		for _, p := range pts {
			area = append(area, int24(p.Lat)...)
			area = append(area, int24(p.Lon)...)
			count++
		}
	}
	if count == 0 {
		kind = AreaNone
	}
	b = append(b, clampByte(kind)<<4|clampByte(count))
	b = append(b, area...)

	// The text: shortened until the whole digest fits.
	budget := MaxDigestBytes - SignatureBytes - len(b) - 1
	if budget < 0 {
		return nil, ErrTooLong
	}
	text := cleanText(d.Text, MaxText)
	packed := compress.Compress([]byte(text))
	for len(packed) > budget && text != "" {
		text = cutRunes(text, utf8.RuneCountInString(text)-max(1, (len(packed)-budget)/2))
		packed = compress.Compress([]byte(text))
	}
	n := len(packed)
	if n > budget || n > 255 {
		return nil, ErrTooLong
	}
	b = append(b, byte(n))
	b = append(b, packed...)
	return b, nil
}

// Decode reads a digest without checking its signature. The signature's
// bytes are answered too, so a caller that verifies first can.
func Decode(wire []byte) (*Digest, []byte, error) {
	if len(wire) < MinDigestBytes || len(wire) > MaxDigestBytes {
		return nil, nil, fmt.Errorf("%w: %d bytes", ErrBadDigest, len(wire))
	}
	if wire[0] != Magic || wire[1] != Version {
		return nil, nil, fmt.Errorf("%w: magic %#x version %d", ErrBadDigest, wire[0], wire[1])
	}
	body, sig := wire[:len(wire)-SignatureBytes], wire[len(wire)-SignatureBytes:]
	d := &Digest{
		MsgType: int(body[2] & 0x0F),
		Test:    body[2]&flagTest != 0,
		Source:  int(body[3]),
	}
	if d.MsgType < TypeAlert || d.MsgType > TypeCancel {
		return nil, nil, fmt.Errorf("%w: message type %d", ErrBadDigest, d.MsgType)
	}
	copy(d.ID[:], body[4:12])
	copy(d.Ref[:], body[12:20])
	d.Sent = fromUnix32(binary.BigEndian.Uint32(body[20:24]))
	d.Expires = fromUnix32(binary.BigEndian.Uint32(body[24:28]))
	d.Severity, d.Urgency = int(body[28]>>4), int(body[28]&0x0F)
	d.Certainty, d.Category = int(body[29]>>4), int(body[29]&0x0F)
	d.Event = int(body[30])
	if body[31] != 0 && body[32] != 0 {
		d.Language = string(body[31:33])
	}
	kind, count := int(body[33]>>4), int(body[33]&0x0F)
	off := headerBytes
	switch kind {
	case AreaNone:
		if count != 0 {
			return nil, nil, fmt.Errorf("%w: area count without an area", ErrBadDigest)
		}
	case AreaGeocodes:
		if count > maxGeocodes || len(body) < off+count*geocodeBytes {
			return nil, nil, fmt.Errorf("%w: geocodes", ErrBadDigest)
		}
		for i := 0; i < count; i++ {
			slot := body[off : off+geocodeBytes]
			d.Geocodes = append(d.Geocodes, Geocode{Type: int(slot[0]), Value: strings.TrimRight(string(slot[1:]), "\x00")})
			off += geocodeBytes
		}
	case AreaPolygon:
		if count < 3 || count > maxPolygon || len(body) < off+count*pointBytes {
			return nil, nil, fmt.Errorf("%w: polygon", ErrBadDigest)
		}
		for i := 0; i < count; i++ {
			d.Polygon = append(d.Polygon, geo.Point{Lat: fromInt24(body[off : off+3]), Lon: fromInt24(body[off+3 : off+6])})
			off += pointBytes
		}
	default:
		return nil, nil, fmt.Errorf("%w: area kind %d", ErrBadDigest, kind)
	}
	if len(body) < off+1 {
		return nil, nil, fmt.Errorf("%w: no text length", ErrBadDigest)
	}
	n := int(body[off])
	off++
	if len(body) != off+n {
		return nil, nil, fmt.Errorf("%w: text length %d, %d bytes left", ErrBadDigest, n, len(body)-off)
	}
	if n > 0 {
		text, err := compress.Decompress(body[off : off+n])
		if err != nil || !utf8.Valid(text) {
			return nil, nil, fmt.Errorf("%w: text", ErrBadDigest)
		}
		d.Text = string(text)
	}
	return d, sig, nil
}

// Verify checks a digest's signature against a PKIX P-256 public key and
// decodes it. The signature is checked over the exact bytes before it is
// read, so a field this version does not know is still covered.
func Verify(pubPKIX, wire []byte) (*Digest, error) {
	if len(wire) < MinDigestBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrBadDigest, len(wire))
	}
	key, err := x509.ParsePKIXPublicKey(pubPKIX)
	if err != nil {
		return nil, fmt.Errorf("warnings: public key: %w", err)
	}
	pub, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("warnings: the public key is not ECDSA")
	}
	body, sig := wire[:len(wire)-SignatureBytes], wire[len(wire)-SignatureBytes:]
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	digest := sha256.Sum256(body)
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return nil, ErrSignature
	}
	d, _, err := Decode(wire)
	return d, err
}

// rawSignature turns an ASN.1 DER ECDSA signature into r || s, 32 bytes each.
func rawSignature(der []byte) ([]byte, error) {
	var sig struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(der, &sig); err != nil {
		return nil, fmt.Errorf("warnings: signature: %w", err)
	}
	out := make([]byte, SignatureBytes)
	rb, sb := sig.R.Bytes(), sig.S.Bytes()
	if len(rb) > 32 || len(sb) > 32 {
		return nil, errors.New("warnings: signature is not P-256")
	}
	copy(out[32-len(rb):32], rb)
	copy(out[64-len(sb):], sb)
	return out, nil
}

func unix32(t time.Time) uint32 {
	if t.IsZero() {
		return 0
	}
	u := t.Unix()
	if u < 0 || u > 0xFFFFFFFF {
		return 0
	}
	return uint32(u)
}

func fromUnix32(u uint32) time.Time {
	if u == 0 {
		return time.Time{}
	}
	return time.Unix(int64(u), 0).UTC()
}

func nibbles(hi, lo int) byte {
	return clampNibble(hi)<<4 | clampNibble(lo)
}

// clampNibble and clampByte hold a code to its field's range: an unknown
// value reads as unknown (0), a value past the table as its last entry.
func clampNibble(v int) byte {
	if v < 0 || v > 15 {
		if v < 0 {
			return 0
		}
		return 15
	}
	return byte(v)
}

func clampByte(v int) byte {
	if v < 0 || v > 255 {
		if v < 0 {
			return 0
		}
		return 255
	}
	return byte(v)
}

// int24 writes degrees * 10000 as a signed 24-bit big-endian value
// (resolution 11 m; latitudes and longitudes both fit).
func int24(deg float64) []byte {
	v := int64(deg * 10000)
	if v > 8388607 {
		v = 8388607
	}
	if v < -8388608 {
		v = -8388608
	}
	u := v & 0xFFFFFF // two's complement, 24 bits, now 0..0xFFFFFF
	if u < 0 || u > 0xFFFFFF {
		u = 0
	}
	hi, mid, lo := u>>16, (u>>8)&0xFF, u&0xFF
	if hi < 0 || hi > 255 || mid < 0 || mid > 255 || lo < 0 || lo > 255 {
		return []byte{0, 0, 0}
	}
	return []byte{byte(hi), byte(mid), byte(lo)}
}

func fromInt24(b []byte) float64 {
	u := int64(b[0])<<16 | int64(b[1])<<8 | int64(b[2])
	if u&0x800000 != 0 {
		u -= 1 << 24
	}
	return float64(u) / 10000
}

// fitPolygon simplifies a polygon until it has at most n points, raising the
// tolerance each pass; a ring that will not simplify is cut.
func fitPolygon(pts []geo.Point, n int) []geo.Point {
	if len(pts) <= n {
		return pts
	}
	eps := 0.001
	for i := 0; i < 20; i++ {
		s := geo.Simplify(pts, eps)
		if len(s) <= n {
			if len(s) >= 3 {
				return s
			}
			break
		}
		eps *= 1.6
	}
	return pts[:n]
}

// cleanText makes one line of at most maxRunes runes.
func cleanText(s string, maxRunes int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || r == ' ':
			space = true
		case r < 0x20 || r == utf8.RuneError:
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		}
	}
	return cutRunes(b.String(), maxRunes)
}

func cutRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimRight(string([]rune(s)[:n]), " ")
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' }

// Code lookups from CAP words.

func severityCode(s string) int   { return indexOf(severities, s) }
func urgencyCode(s string) int    { return indexOf(urgencies, s) }
func certaintyCode(s string) int  { return indexOf(certainties, s) }
func categoryCode(s []string) int { return firstIndexOf(categories, s) }
func SeverityName(c int) string   { return nameAt(severities, c) }
func UrgencyName(c int) string    { return nameAt(urgencies, c) }
func CertaintyName(c int) string  { return nameAt(certainties, c) }
func CategoryName(c int) string   { return nameAt(categories, c) }
func indexOf(t []string, s string) int {
	for i, v := range t {
		if strings.EqualFold(v, strings.TrimSpace(s)) {
			return i
		}
	}
	return 0
}

func firstIndexOf(t []string, ss []string) int {
	for _, s := range ss {
		if i := indexOf(t, s); i != 0 {
			return i
		}
	}
	return 0
}

func nameAt(t []string, c int) string {
	if c < 0 || c >= len(t) {
		return t[0]
	}
	return t[c]
}

// EventCode maps an info block's codes and words to the event table.
func EventCode(info *Info) int {
	if info == nil {
		return EventOther
	}
	// MeteoAlarm: awareness_type "1; Wind".
	for _, p := range info.Parameter {
		if strings.EqualFold(p.Name, "awareness_type") {
			if n, _, ok := strings.Cut(p.Value, ";"); ok {
				switch strings.TrimSpace(n) {
				case "1":
					return EventWind
				case "2":
					return EventSnowIce
				case "3":
					return EventThunderstorm
				case "4":
					return EventFog
				case "5":
					return EventHeat
				case "6":
					return EventCold
				case "7":
					return EventCoastal
				case "8":
					return EventWildfire
				case "9":
					return EventAvalanche
				case "10":
					return EventRain
				case "12", "13":
					return EventFlood
				}
			}
		}
	}
	// DWD: eventCode GROUP.
	for _, e := range info.EventCode {
		if strings.EqualFold(e.Name, "GROUP") {
			switch strings.ToUpper(e.Value) {
			case "WIND", "STORM":
				return EventWind
			case "RAIN":
				return EventRain
			case "SNOWFALL", "SNOW", "ICE", "GLAZE", "GLATTEIS":
				return EventSnowIce
			case "THUNDERSTORM":
				return EventThunderstorm
			case "FOG":
				return EventFog
			case "HEAT", "UV":
				return EventHeat
			case "FROST", "COLD":
				return EventCold
			case "COAST", "COASTAL", "STORMSURGE":
				return EventCoastal
			case "FLOOD", "FLOODING":
				return EventFlood
			case "TORNADO":
				return EventTornado
			}
		}
	}
	return eventFromWords(info.Event + " " + info.Headline)
}

// eventFromWords reads the event from English, Dutch and German words.
func eventFromWords(s string) int {
	s = strings.ToLower(s)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(s, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("tornado"):
		return EventTornado
	case has("thunder", "onweer", "gewitter"):
		return EventThunderstorm
	case has("wind", "storm", "sturm", "böen", "boeen", "gust", "orkan"):
		return EventWind
	case has("snow", "sneeuw", "schnee", "ice", "ijzel", "glätte", "glatt", "frost", "vorst"):
		return EventSnowIce
	case has("rain", "regen", "neerslag", "dauerregen"):
		return EventRain
	case has("fog", "mist", "nebel"):
		return EventFog
	case has("heat", "hitte", "hitze"):
		return EventHeat
	case has("cold", "kou", "kälte"):
		return EventCold
	case has("coast", "kust", "küste", "surge", "hochwasser an der küste"):
		return EventCoastal
	case has("flood", "overstroming", "hochwasser"):
		return EventFlood
	case has("avalanche", "lawine"):
		return EventAvalanche
	case has("fire", "brand", "waldbrand"):
		return EventWildfire
	}
	return EventOther
}

// FromAlert builds a digest from a CAP alert, taking the info block in the
// wanted languages. source is a Source* id.
func FromAlert(a *Alert, source int, languages []string) *Digest {
	d := &Digest{Source: source, Test: a.IsTest(), ID: IDOf(a.Identifier), Sent: a.Sent}
	switch strings.ToLower(a.MsgType) {
	case "update":
		d.MsgType = TypeUpdate
	case "cancel":
		d.MsgType = TypeCancel
	default:
		d.MsgType = TypeAlert
	}
	if d.MsgType != TypeAlert && len(a.References) > 0 {
		d.Ref = IDOf(a.References[len(a.References)-1].Identifier)
	}
	d.Expires = a.Expires()
	info := a.InfoFor(languages)
	if info == nil {
		return d
	}
	d.Severity, d.Urgency, d.Certainty = severityCode(info.Severity), urgencyCode(info.Urgency), certaintyCode(info.Certainty)
	d.Category = categoryCode(info.Category)
	d.Event = EventCode(info)
	d.Language = info.Language
	d.Text = info.Headline
	if d.Text == "" {
		d.Text = info.Event
	}
	for _, ar := range info.Areas {
		for _, g := range ar.Geocodes {
			d.Geocodes = append(d.Geocodes, Geocode{Type: geocodeType(g.Name), Value: g.Value})
		}
	}
	if len(d.Geocodes) == 0 {
		// One polygon only: the largest ring, simplified by the encoder.
		for _, ar := range info.Areas {
			for _, p := range ar.Polygons {
				if len(p) > len(d.Polygon) {
					d.Polygon = p
				}
			}
		}
	}
	return d
}

func geocodeType(name string) int {
	switch strings.ToUpper(name) {
	case "WARNCELLID":
		return GeocodeWarnCellID
	case "EMMA_ID":
		return GeocodeEMMAID
	case "NUTS", "NUTS3", "NUTS2":
		return GeocodeNUTS
	}
	return GeocodeOther
}
