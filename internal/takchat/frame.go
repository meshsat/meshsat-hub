package takchat

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/meshsat/meshsat-hub/internal/compress"
	"github.com/meshsat/meshsat-hub/internal/hubmsg"
	"github.com/meshsat/meshsat-hub/internal/tak"
)

// Crew chat over satellite and SMS (MESHSAT-1744). A kit off MQTT gets a
// dispatcher's chat as one compact frame, a satellite MT or an SMS, and
// answers the same way. The layouts are the contract
// (docs/bridge-hub-protocol.md, "Crew chat over satellite and SMS"); the
// Bridge's internal/crewchat reads and writes the same bytes.
//
//	C1, Hub to kit (a satellite MT, or base64 in one SMS from the Hub's number):
//	  'C' | flags | message id (16) | crew node (4, BE) | operator hash (8) |
//	  callsign (1 + up to 16) | text (SMAZ2 when flag 0x01, else UTF-8)
//	C2, kit to Hub, the crew's answer (an uplink frame, type 0x43):
//	  4D 53 01 43 | message id (16) | crew node (4) | unix seconds (4) | flags | text
//	C3, kit to Hub, what became of the chat (an uplink frame, type 0x44):
//	  4D 53 01 44 | message id (16) | crew node (4) | unix seconds (4) |
//	  status (1) | reason (1) | detail (1 + up to 80)
//
// The message id is the GeoChat's own, so the kit reports on it and the
// crew's answer is filed against it; the operator hash is the first eight
// bytes of SHA-256 of the operator's uid, which the kit writes back as
// "meshsat-hub-op-~<16 hex>" when it answers over MQTT instead, and the Hub
// resolves (OperatorFromHash).

const (
	// MTTypeChat is the first byte of a C1 frame: 'C', outside the kit's
	// compact-binary flag values and the warning digest's 'W'.
	MTTypeChat byte = 0x43
	// UplinkTypeAnswer and UplinkTypeStatus are the uplink frame types of
	// C2 and C3 (bridge.SatMsgCrewAnswer, bridge.SatMsgCrewStatus).
	UplinkTypeAnswer byte = 0x43
	UplinkTypeStatus byte = 0x44

	// C1 flags.
	FlagCompressed byte = 0x01 // the text is SMAZ2 (Meshtastic table)
	FlagToKit      byte = 0x02 // the crew is the kit itself (uid MESHSAT-<node hex>)
	FlagReplyHere  byte = 0x04 // answer and status on this bearer even with the Hub link up

	// C2 flags.
	AnswerFlagCompressed byte = 0x01

	// Frame field bounds.
	maxFrameCallsign = 16
	maxFrameDetail   = 80

	// What one frame may be on each bearer: an SMS carries 160 GSM
	// characters, 120 bytes in base64; an SBD MT is 270 bytes; an IMT MT
	// takes more, bounded as the "@" lane bounds its replies.
	MaxFrameSMS = 120
	MaxFrameSBD = 270
	MaxFrameIMT = 1024

	// OperatorHashMarker follows tak.OperatorUIDPrefix in the uid a kit
	// writes for an operator it knows by hash alone.
	OperatorHashMarker = "~"
)

// C3 status and reason codes, the Bridge's words as one byte each.
const (
	StatusCodeQueued      byte = 1
	StatusCodeDelivered   byte = 2
	StatusCodeFailed      byte = 3
	StatusCodeUndelivered byte = 4
)

var statusWords = map[byte]string{
	StatusCodeQueued: StatusQueued, StatusCodeDelivered: StatusDelivered,
	StatusCodeFailed: StatusFailed, StatusCodeUndelivered: StatusUndelivered,
}

var reasonWords = map[byte]string{
	1: "not_heard", 2: "ambiguous_callsign", 3: "no_radio", 4: "no_text",
	5: "no_sender", 6: "own_chat", 7: "send_failed",
}

// ReasonCode is the byte for a reason word; 0 for none or one not listed.
func ReasonCode(word string) byte {
	for code, w := range reasonWords {
		if w == word {
			return code
		}
	}
	return 0
}

// Frame errors.
var (
	ErrFrameShort   = errors.New("takchat: frame too short")
	ErrFrameType    = errors.New("takchat: not a crew chat frame")
	ErrFrameText    = errors.New("takchat: the frame's text does not decompress")
	ErrFrameID      = errors.New("takchat: the message id is not a UUID")
	ErrFrameCrew    = errors.New("takchat: the crew cannot be named in a frame")
	ErrFrameTooLong = errors.New("takchat: too long for the bearer")
)

// Chat is a C1 frame's content.
type Chat struct {
	MessageID    string // the GeoChat's message id, canonical UUID text
	Node         uint32 // the crew's mesh node id, or the kit's own
	ToKit        bool   // the crew is the kit itself
	ReplyHere    bool   // answer and status on the bearer the chat came on
	OperatorUID  string // encoded as its hash; empty when decoded
	OperatorHash [8]byte
	Callsign     string // the operator's name as the crew sees it
	Text         string
}

// OperatorHash is the eight bytes a frame carries for an operator's uid.
func OperatorHash(uid string) [8]byte {
	sum := sha256.Sum256([]byte(uid))
	var h [8]byte
	copy(h[:], sum[:8])
	return h
}

// OperatorUIDFromHash is the uid a kit writes for an operator it knows by
// hash: tak.OperatorUIDPrefix, the marker, 16 hex digits.
func OperatorUIDFromHash(h [8]byte) string {
	return tak.OperatorUIDPrefix + OperatorHashMarker + hex.EncodeToString(h[:])
}

// HashFromOperatorUID reads the hash back out of such a uid; ok is false for
// any other uid, an operator's real one included.
func HashFromOperatorUID(uid string) (h [8]byte, ok bool) {
	rest, found := strings.CutPrefix(uid, tak.OperatorUIDPrefix+OperatorHashMarker)
	if !found || len(rest) != 16 {
		return h, false
	}
	b, err := hex.DecodeString(rest)
	if err != nil {
		return h, false
	}
	copy(h[:], b)
	return h, true
}

// CrewNode reads the node a crew uid names: "meshsat-<8 hex>" for a mesh
// node behind the kit, "MESHSAT-<8 hex>" for the kit itself (toKit true).
func CrewNode(uid string) (node uint32, toKit bool, ok bool) {
	if h, found := strings.CutPrefix(uid, "meshsat-"); found && len(h) == 8 {
		if v, err := strconv.ParseUint(h, 16, 32); err == nil {
			return uint32(v), false, true
		}
	}
	if h, found := strings.CutPrefix(uid, "MESHSAT-"); found && len(h) == 8 {
		if v, err := strconv.ParseUint(h, 16, 32); err == nil {
			return uint32(v), true, true
		}
	}
	return 0, false, false
}

// CrewUIDOf is the uid a frame's node and flag stand for.
func CrewUIDOf(node uint32, toKit bool) string {
	if toKit {
		return fmt.Sprintf("MESHSAT-%08x", node)
	}
	return fmt.Sprintf("meshsat-%08x", node)
}

// EncodeChat writes a C1 frame. The text goes SMAZ2 when that is shorter.
func EncodeChat(c Chat) ([]byte, error) {
	id, ok := hubmsg.UUIDBytes(c.MessageID)
	if !ok {
		return nil, ErrFrameID
	}
	flags := byte(0)
	if c.ToKit {
		flags |= FlagToKit
	}
	if c.ReplyHere {
		flags |= FlagReplyHere
	}
	text := []byte(c.Text)
	if packed := compress.Compress(text); len(packed) > 0 && len(packed) < len(text) {
		if back, err := compress.Decompress(packed); err == nil && string(back) == c.Text {
			text, flags = packed, flags|FlagCompressed
		}
	}
	hash := c.OperatorHash
	if c.OperatorUID != "" {
		hash = OperatorHash(c.OperatorUID)
	}
	callsign := cutRunes(c.Callsign, maxFrameCallsign)
	out := make([]byte, 0, 2+16+4+8+1+len(callsign)+len(text))
	out = append(out, MTTypeChat, flags)
	out = append(out, id[:]...)
	out = binary.BigEndian.AppendUint32(out, c.Node)
	out = append(out, hash[:]...)
	out = append(out, byte(len(callsign))) // #nosec G115 -- cut to maxFrameCallsign above
	out = append(out, callsign...)
	out = append(out, text...)
	return out, nil
}

// IsChatFrame reports whether data starts like a C1 frame.
func IsChatFrame(data []byte) bool {
	return len(data) >= 31 && data[0] == MTTypeChat
}

// DecodeChat reads a C1 frame.
func DecodeChat(data []byte) (Chat, error) {
	var c Chat
	if len(data) < 31 {
		return c, ErrFrameShort
	}
	if data[0] != MTTypeChat {
		return c, ErrFrameType
	}
	flags := data[1]
	var id [16]byte
	copy(id[:], data[2:18])
	c.MessageID = hubmsg.UUIDText(id)
	c.Node = binary.BigEndian.Uint32(data[18:22])
	copy(c.OperatorHash[:], data[22:30])
	c.ToKit = flags&FlagToKit != 0
	c.ReplyHere = flags&FlagReplyHere != 0
	n := int(data[30])
	if n > maxFrameCallsign || len(data) < 31+n {
		return c, ErrFrameShort
	}
	c.Callsign = string(data[31 : 31+n])
	text := data[31+n:]
	if flags&FlagCompressed != 0 {
		plain, err := compress.Decompress(text)
		if err != nil {
			return c, ErrFrameText
		}
		text = plain
	}
	if !utf8.Valid(text) {
		return c, ErrFrameText
	}
	c.Text = string(text)
	return c, nil
}

// Answer is a C2 frame's content: the crew's words on one question.
type Answer struct {
	MessageID string // the question's
	Node      uint32
	At        time.Time
	Text      string
}

// EncodeAnswer writes a C2 frame's payload, after the uplink header.
func EncodeAnswer(a Answer) ([]byte, error) {
	id, ok := hubmsg.UUIDBytes(a.MessageID)
	if !ok {
		return nil, ErrFrameID
	}
	flags := byte(0)
	text := []byte(a.Text)
	if packed := compress.Compress(text); len(packed) > 0 && len(packed) < len(text) {
		if back, err := compress.Decompress(packed); err == nil && string(back) == a.Text {
			text, flags = packed, AnswerFlagCompressed
		}
	}
	out := make([]byte, 0, 16+4+4+1+len(text))
	out = append(out, id[:]...)
	out = binary.BigEndian.AppendUint32(out, a.Node)
	out = binary.BigEndian.AppendUint32(out, uint32(a.At.Unix())) // #nosec G115 -- unix seconds until 2106
	out = append(out, flags)
	out = append(out, text...)
	return out, nil
}

// DecodeAnswer reads a C2 frame's payload.
func DecodeAnswer(payload []byte) (Answer, error) {
	var a Answer
	if len(payload) < 25 {
		return a, ErrFrameShort
	}
	var id [16]byte
	copy(id[:], payload[:16])
	a.MessageID = hubmsg.UUIDText(id)
	a.Node = binary.BigEndian.Uint32(payload[16:20])
	a.At = time.Unix(int64(binary.BigEndian.Uint32(payload[20:24])), 0).UTC()
	text := payload[25:]
	if payload[24]&AnswerFlagCompressed != 0 {
		plain, err := compress.Decompress(text)
		if err != nil {
			return a, ErrFrameText
		}
		text = plain
	}
	if !utf8.Valid(text) {
		return a, ErrFrameText
	}
	a.Text = string(text)
	return a, nil
}

// Status is a C3 frame's content: the kit's word on one chat.
type Status struct {
	MessageID string
	Node      uint32
	At        time.Time
	Status    string // one of the four words
	Reason    string // one machine word, "" for none
	Detail    string
}

// EncodeStatus writes a C3 frame's payload, after the uplink header.
func EncodeStatus(s Status) ([]byte, error) {
	id, ok := hubmsg.UUIDBytes(s.MessageID)
	if !ok {
		return nil, ErrFrameID
	}
	code := byte(0)
	for c, w := range statusWords {
		if w == s.Status {
			code = c
		}
	}
	if code == 0 {
		return nil, fmt.Errorf("takchat: status %q is not one of the four words", s.Status)
	}
	detail := cutRunes(s.Detail, maxFrameDetail)
	out := make([]byte, 0, 16+4+4+1+1+1+len(detail))
	out = append(out, id[:]...)
	out = binary.BigEndian.AppendUint32(out, s.Node)
	out = binary.BigEndian.AppendUint32(out, uint32(s.At.Unix()))    // #nosec G115 -- unix seconds until 2106
	out = append(out, code, ReasonCode(s.Reason), byte(len(detail))) // #nosec G115 -- cut to maxFrameDetail above
	out = append(out, detail...)
	return out, nil
}

// DecodeStatus reads a C3 frame's payload.
func DecodeStatus(payload []byte) (Status, error) {
	var s Status
	if len(payload) < 27 {
		return s, ErrFrameShort
	}
	var id [16]byte
	copy(id[:], payload[:16])
	s.MessageID = hubmsg.UUIDText(id)
	s.Node = binary.BigEndian.Uint32(payload[16:20])
	s.At = time.Unix(int64(binary.BigEndian.Uint32(payload[20:24])), 0).UTC()
	word, ok := statusWords[payload[24]]
	if !ok {
		return s, fmt.Errorf("takchat: status code %d is not one of the four", payload[24])
	}
	s.Status = word
	s.Reason = reasonWords[payload[25]]
	n := int(payload[26])
	if n > maxFrameDetail || len(payload) < 27+n {
		return s, ErrFrameShort
	}
	detail := payload[27 : 27+n]
	if !utf8.Valid(detail) {
		return s, ErrFrameText
	}
	s.Detail = string(detail)
	return s, nil
}

// cutRunes cuts s to at most n bytes on a character boundary.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// MaxFrame is the most a frame may be on a bearer.
func MaxFrame(bearer string) int {
	switch bearer {
	case BearerSMS:
		return MaxFrameSMS
	case BearerSBD:
		return MaxFrameSBD
	case BearerIMT:
		return MaxFrameIMT
	}
	return 0
}

// hubmsgUUID is hubmsg.UUIDText, named for the answer id derivation.
func hubmsgUUID(b [16]byte) string { return hubmsg.UUIDText(b) }
