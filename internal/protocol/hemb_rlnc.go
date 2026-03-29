package protocol

// HeMB RLNC decoding — ported from meshsat/internal/hemb/rlnc.go (decode-only).

import (
	"errors"
	"fmt"
)

// HeMBCodedSymbol is a single RLNC-coded symbol.
type HeMBCodedSymbol struct {
	GenID        uint16
	SymbolIndex  int
	K            int    // number of source segments
	Coefficients []byte // K bytes — GF(256) coefficients
	Data         []byte // coded payload
}

// ErrHeMBNotDecodable is returned when TryDecode has insufficient independent symbols.
var ErrHeMBNotDecodable = errors.New("hemb: not decodable — insufficient independent symbols")

// HeMBTryDecode attempts Gaussian elimination on received coded symbols.
// Returns K decoded segment payloads on success.
func HeMBTryDecode(symbols []HeMBCodedSymbol, k int) ([][]byte, error) {
	if len(symbols) < k {
		return nil, fmt.Errorf("%w: have %d symbols, need %d", ErrHeMBNotDecodable, len(symbols), k)
	}

	n := len(symbols)

	coeffs := newHeMBGFMatrix(n, k)
	payloads := make([][]byte, n)
	for i, sym := range symbols {
		for j := 0; j < k; j++ {
			if j < len(sym.Coefficients) {
				coeffs.set(i, j, sym.Coefficients[j])
			}
		}
		payloads[i] = sym.Data
	}

	decoded, err := hembGaussianEliminate(coeffs, payloads)
	if err != nil {
		if errors.Is(err, ErrHeMBRankDeficient) {
			return nil, fmt.Errorf("%w: %v", ErrHeMBNotDecodable, err)
		}
		return nil, err
	}

	return decoded, nil
}
