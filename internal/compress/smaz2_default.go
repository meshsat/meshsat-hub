package compress

import "github.com/lib-x/smaz2"

// The default SMAZ2 table (lib-x smaz2): what MeshSat Android, MeshSat iOS
// and the Bridge compress an SMS with. The Hub only knew the Meshtastic table,
// so it read their word codes from the wrong list ("this is a test of the
// message" came out "node is a test of the camp"). [MESHSAT-1646]

// CompressDefault compresses with the default table byte for byte as the
// Bridge and both apps do, a byte 0x01-0x08 as its own verbatim run
// (MESHSAT-1626): lib-x writes such a byte as a literal its decoder reads as
// an escape. Text without one is exactly lib-x's output.
func CompressDefault(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	var out []byte
	start := 0
	for i, b := range data {
		if b >= 0x01 && b <= 0x08 {
			if start < i {
				out = append(out, smaz2.CompressBytes(data[start:i])...)
			}
			out = append(out, 0x01, b)
			start = i + 1
		}
	}
	if start == 0 {
		return smaz2.CompressBytes(data)
	}
	if start < len(data) {
		out = append(out, smaz2.CompressBytes(data[start:])...)
	}
	return out
}

// DecompressDefault reads default-table SMAZ2.
func DecompressDefault(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	return smaz2.DecompressToBytes(data)
}
