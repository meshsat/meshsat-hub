package sms

import (
	"bytes"
	"unicode/utf8"

	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/compress"
)

// smsText reads the text of a version 1 or unversioned binary SMS payload
// once its version byte and encryption are off, as MeshSat Android and iOS do
// (SmsReceiver.smaz2Text, SmsWire.decode). Version 1 does not say which codec
// a payload is in, so this is a rule, and it cannot be right for every
// payload: the SMAZ2 of "sara" is the two bytes of "¡", and reads as "¡".
// Senders of wire v2 say which (readSMSWireV2). [MESHSAT-1646]
//
//   - A payload that is itself readable UTF-8 with any non-ASCII character is
//     the text, never SMAZ2. The senders compress only when it gains, so
//     Greek, accented or emoji text mostly goes out uncompressed, and read as
//     SMAZ2 every byte above 0x7F of it is a bigram: "Καλημέρα" came out as
//     ASCII soup, and compressing that soup again gives the same bytes, so
//     no round trip can tell. A compressed payload is almost never readable
//     UTF-8 with non-ASCII in it: its verbatim counts (1-5) and word codes
//     (6-8) are control bytes, its bigram codes mostly stray bytes above 0x7F.
//   - Otherwise SMAZ2 with the default table, the senders' (the Hub used the
//     Meshtastic table: "this is a test of the message" came out "node is a
//     test of the camp"), then the Meshtastic table; each only if the text is
//     readable and compressing it again gives exactly these bytes.
//   - MSVQ-SC and plain UTF-8 come after, as before.
func smsText(payload []byte, msvqscDecode func([]byte) (string, bool)) (text string, compressed bool, compressionType string) {
	if plainNonASCII(payload) {
		return string(payload), false, ""
	}
	if t, ok := smaz2Text(payload, compress.DecompressDefault, compress.CompressDefault); ok {
		return t, true, "smaz2"
	}
	if t, ok := smaz2Text(payload, compress.Decompress, compress.Compress); ok {
		return t, true, "smaz2"
	}
	if msvqscDecode != nil {
		if t, ok := msvqscDecode(payload); ok {
			return t, true, "msvqsc"
		}
	}
	if utf8.Valid(payload) {
		return string(payload), false, ""
	}
	return "", false, ""
}

func smaz2Text(payload []byte, decompress func([]byte) ([]byte, error), recompress func([]byte) []byte) (string, bool) {
	if len(payload) == 0 {
		return "", false
	}
	out, err := decompress(payload)
	if err != nil || len(out) == 0 || !utf8.Valid(out) || !readable(string(out)) {
		return "", false
	}
	if !bytes.Equal(recompress(out), payload) {
		return "", false
	}
	return string(out), true
}

// plainNonASCII: readable UTF-8 text with at least one character above ASCII.
func plainNonASCII(b []byte) bool {
	if !utf8.Valid(b) || !readable(string(b)) {
		return false
	}
	for _, c := range b {
		if c >= 0x80 {
			return true
		}
	}
	return false
}

// readable is the apps' rule: tab, CR, LF and every rune from U+0020 up
// except DEL, the C1 controls and U+FFFD.
func readable(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) || r == utf8.RuneError {
			return false
		}
	}
	return true
}

// smsWireV2Result is what an app SMS in wire v2 reads as (codec.SMSWireV2,
// r40 review R40-R1).
type smsWireV2Result struct {
	text        string
	compression string
	encrypted   bool
	// read: the frame was opened and its codec read. kept: the frame is v2
	// but its mode or codec is one this Hub does not read, or no key opens
	// it; nothing is read into it.
	read, kept bool
}

// readSMSWireV2 reads an app SMS in wire v2. open tries the sender's keys on
// a sealed payload. A blob that is not v2 answers neither read nor kept, and
// the version 1 rule (smsText) reads it.
func readSMSWireV2(raw []byte, open func([]byte) ([]byte, bool), msvqscDecode func([]byte) (string, bool)) smsWireV2Result {
	mode, payload, ok := codec.ParseSMSWireV2(raw)
	if !ok {
		return smsWireV2Result{}
	}
	var frame []byte
	var r smsWireV2Result
	switch mode {
	case codec.SMSModeClear:
		frame = payload
	case codec.SMSModeAESGCM:
		opened, ok := open(payload)
		if !ok {
			return smsWireV2Result{kept: true}
		}
		frame, r.encrypted = opened, true
	default:
		return smsWireV2Result{kept: true}
	}
	if len(frame) == 0 {
		return smsWireV2Result{kept: true, encrypted: r.encrypted}
	}
	body := frame[1:]
	switch frame[0] {
	case codec.SMSCodecText:
		if !utf8.Valid(body) {
			return smsWireV2Result{kept: true, encrypted: r.encrypted}
		}
		r.text = string(body)
	case codec.SMSCodecSMAZ2:
		out, err := compress.DecompressDefault(body)
		if err != nil || !utf8.Valid(out) {
			return smsWireV2Result{kept: true, encrypted: r.encrypted}
		}
		r.text, r.compression = string(out), "smaz2"
	case codec.SMSCodecMSVQSC:
		if msvqscDecode == nil {
			return smsWireV2Result{kept: true, encrypted: r.encrypted}
		}
		t, ok := msvqscDecode(body)
		if !ok {
			return smsWireV2Result{kept: true, encrypted: r.encrypted}
		}
		r.text, r.compression = t, "msvqsc"
	default:
		return smsWireV2Result{kept: true, encrypted: r.encrypted}
	}
	r.read = true
	return r
}
