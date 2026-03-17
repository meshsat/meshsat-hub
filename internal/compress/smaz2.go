package compress

import "fmt"

// SMAZ2 encoding scheme (compatible with meshsat bridge firmware):
//
//	Byte value    Meaning
//	-----------   -------
//	0             Literal byte 0x00
//	1-5           Verbatim: next N bytes are raw literals
//	6             Word escape: next byte is word index
//	7             Word+space: next byte is word index, append space
//	8             Space+word: next byte is word index, prepend space
//	9-127         Literal byte (self-representing)
//	128-255       Bigram: index = (byte & 0x7F), lookup bigrams[index*2:index*2+2]

// meshtasticBigrams contains 128 common letter pairs (256 chars = 128 bigrams).
// MUST match meshsat/internal/compress/dict_meshtastic.go exactly.
const meshtasticBigrams = "intherreheanonesorteattistenntartondalitseediseangoulecomeneriroderaioicliofasetvetasihamaecomceelllcaurlachhidihofonsotacnarssoprrtsassusnoiltsemctgeloeebetrnipeiepancpooldaadviunamutwimoshyoaiewowosfiepttmiopiaweagsuiddoooirspplscaywaigeirylytuulivimabty"

// meshtasticWords contains 256 words optimized for Meshtastic/satellite/field
// communications. MUST match meshsat/internal/compress/dict_meshtastic.go exactly.
var meshtasticWords = []string{
	// Core mesh/radio terms (0-31)
	"mesh", "node", "message", "channel", "signal", "radio", "frequency", "power",
	"antenna", "range", "gateway", "relay", "bridge", "network", "packet", "payload",
	"status", "battery", "voltage", "level", "percent", "position", "latitude", "longitude",
	"altitude", "heading", "speed", "distance", "bearing", "elevation", "terrain", "route",

	// Satellite/Iridium terms (32-63)
	"satellite", "iridium", "modem", "serial", "port", "device", "sensor", "timestamp",
	"meshtastic", "location", "coordinates", "waypoint", "temperature", "humidity", "pressure", "weather",
	"connected", "offline", "online", "update", "received", "transmitted", "error", "warning",
	"critical", "enabled", "disabled", "configuration", "settings", "interface", "protocol", "timeout",

	// Field/SAR terms (64-95)
	"emergency", "rescue", "medical", "assistance", "helicopter", "vehicle", "boat", "shelter",
	"water", "food", "camp", "base", "standing", "moving", "stopped", "request",
	"respond", "acknowledge", "confirmed", "denied", "approved", "copy", "over", "roger",
	"affirmative", "negative", "clear", "blocked", "north", "south", "east", "west",

	// Status/state terms (96-127)
	"success", "failed", "pending", "queued", "delivered", "expired", "forwarded", "dropped",
	"retry", "noise", "interference", "coverage", "degrees", "meters", "kilometers", "knots",
	"duration", "interval", "threshold", "maximum", "minimum", "average", "total", "count",
	"report", "checksum", "baud", "cellular", "astrocast", "webhook", "mqtt", "iridium",

	// Common English (128-191) — high frequency in any text
	"that", "this", "with", "from", "your", "have", "more", "will",
	"home", "about", "time", "they", "what", "which", "their", "there",
	"only", "when", "here", "also", "help", "been", "would", "were",
	"some", "these", "like", "than", "find", "back", "just", "over",
	"into", "them", "should", "then", "good", "well", "where", "right",
	"high", "through", "each", "very", "read", "need", "many", "said",
	"does", "under", "full", "part", "could", "great", "send", "type",
	"because", "local", "those", "using", "result", "before", "make", "data",

	// More common + technical (192-255)
	"area", "want", "show", "even", "check", "open", "today", "state",
	"both", "down", "system", "three", "total", "place", "without", "access",
	"think", "current", "control", "change", "small", "rate", "number", "line",
	"name", "list", "work", "last", "next", "used", "free", "other",
	"first", "world", "after", "best", "long", "based", "code", "being",
	"while", "care", "same", "found", "link", "text", "site", "form",
	"event", "love", "main", "still", "information", "service", "people", "year",
	"email", "group", "detail", "design", "board", "point", "test", "track",
}

// wordIndex maps words to their index for O(1) lookup during compression.
var meshtasticWordIndex map[string]int

func init() {
	if len(meshtasticWords) != 256 {
		panic(fmt.Sprintf("meshtasticWords must have exactly 256 entries, got %d", len(meshtasticWords)))
	}
	meshtasticWordIndex = make(map[string]int, len(meshtasticWords))
	for i, w := range meshtasticWords {
		meshtasticWordIndex[w] = i
	}
}

// Compress compresses text using the Meshtastic SMAZ2 dictionary.
func Compress(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	return compressMeshtastic(data)
}

// Decompress decompresses SMAZ2 data using the Meshtastic dictionary.
func Decompress(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	return decompressMeshtastic(data)
}

// CompressString is a convenience wrapper for string input.
func CompressString(s string) []byte {
	return Compress([]byte(s))
}

// DecompressString is a convenience wrapper returning a string.
func DecompressString(data []byte) (string, error) {
	b, err := Decompress(data)
	return string(b), err
}

func compressMeshtastic(s []byte) []byte {
	dst := make([]byte, 0, len(s))
	verbatimStart := -1

	flushVerbatim := func() {
		verbatimStart = -1
	}

	for pos := 0; pos < len(s); {
		if pos < len(s) {
			// Check space+word (byte 8).
			if s[pos] == ' ' && pos+1 < len(s) {
				bestLen := 0
				bestIdx := 0
				for end := pos + 1; end <= len(s) && end-pos-1 <= 40; end++ {
					candidate := string(s[pos+1 : end])
					if idx, ok := meshtasticWordIndex[candidate]; ok {
						if len(candidate) > bestLen {
							bestLen = len(candidate)
							bestIdx = idx
						}
					}
				}
				if bestLen >= 4 {
					flushVerbatim()
					dst = append(dst, 8, byte(bestIdx))
					pos += 1 + bestLen
					continue
				}
			}

			// Check word+space (byte 7) and plain word (byte 6).
			bestLen := 0
			bestIdx := 0
			maxLen := len(s) - pos
			if maxLen > 40 {
				maxLen = 40
			}
			for end := pos + 4; end <= pos+maxLen; end++ {
				candidate := string(s[pos:end])
				if idx, ok := meshtasticWordIndex[candidate]; ok {
					if len(candidate) > bestLen {
						bestLen = len(candidate)
						bestIdx = idx
					}
				}
			}
			if bestLen >= 4 {
				flushVerbatim()
				if pos+bestLen < len(s) && s[pos+bestLen] == ' ' {
					dst = append(dst, 7, byte(bestIdx))
					pos += bestLen + 1
				} else {
					dst = append(dst, 6, byte(bestIdx))
					pos += bestLen
				}
				continue
			}
		}

		// Try bigram match.
		if pos+1 < len(s) {
			bigramFound := false
			b0, b1 := s[pos], s[pos+1]
			for i := 0; i < len(meshtasticBigrams); i += 2 {
				if meshtasticBigrams[i] == b0 && meshtasticBigrams[i+1] == b1 {
					flushVerbatim()
					dst = append(dst, 0x80|byte(i/2))
					pos += 2
					bigramFound = true
					break
				}
			}
			if bigramFound {
				continue
			}
		}

		// Safe literal (0 or 9-127).
		ch := s[pos]
		if ch == 0 || (ch >= 9 && ch < 128) {
			flushVerbatim()
			dst = append(dst, ch)
			pos++
			continue
		}

		// Verbatim byte (1-8 or >= 128).
		if verbatimStart >= 0 && dst[verbatimStart] < 5 {
			dst[verbatimStart]++
			dst = append(dst, ch)
			if dst[verbatimStart] == 5 {
				flushVerbatim()
			}
		} else {
			verbatimStart = len(dst)
			dst = append(dst, 1, ch)
		}
		pos++
	}

	return dst
}

func decompressMeshtastic(c []byte) ([]byte, error) {
	res := make([]byte, 0, len(c)*2)
	i := 0
	for i < len(c) {
		b := c[i]
		switch {
		case b >= 128:
			idx := int(b & 0x7F)
			if idx*2+1 >= len(meshtasticBigrams) {
				return nil, fmt.Errorf("smaz2: bigram index %d out of range", idx)
			}
			res = append(res, meshtasticBigrams[idx*2], meshtasticBigrams[idx*2+1])
			i++
		case b >= 1 && b <= 5:
			n := int(b)
			if i+1+n > len(c) {
				return nil, fmt.Errorf("smaz2: verbatim length %d overflows at position %d", n, i)
			}
			res = append(res, c[i+1:i+1+n]...)
			i += 1 + n
		case b == 6:
			if i+1 >= len(c) {
				return nil, fmt.Errorf("smaz2: word escape at end of input")
			}
			idx := int(c[i+1])
			if idx >= len(meshtasticWords) {
				return nil, fmt.Errorf("smaz2: word index %d out of range", idx)
			}
			res = append(res, meshtasticWords[idx]...)
			i += 2
		case b == 7:
			if i+1 >= len(c) {
				return nil, fmt.Errorf("smaz2: word+space escape at end of input")
			}
			idx := int(c[i+1])
			if idx >= len(meshtasticWords) {
				return nil, fmt.Errorf("smaz2: word index %d out of range", idx)
			}
			res = append(res, meshtasticWords[idx]...)
			res = append(res, ' ')
			i += 2
		case b == 8:
			if i+1 >= len(c) {
				return nil, fmt.Errorf("smaz2: space+word escape at end of input")
			}
			idx := int(c[i+1])
			if idx >= len(meshtasticWords) {
				return nil, fmt.Errorf("smaz2: word index %d out of range", idx)
			}
			res = append(res, ' ')
			res = append(res, meshtasticWords[idx]...)
			i += 2
		default:
			res = append(res, b)
			i++
		}
	}
	return res, nil
}
