package protocol

// GF(256) arithmetic for HeMB RLNC decoding.
// Ported from meshsat/internal/hemb/gf256.go (decode-only subset).
// Irreducible polynomial: x^8 + x^4 + x^3 + x + 1 (0x11B).

import (
	"errors"
	"fmt"
)

var hembExpTable [512]byte
var hembLogTable [256]byte

func init() { initHeMBGFTables() }

func initHeMBGFTables() {
	var x uint16 = 1
	for i := 0; i < 255; i++ {
		hembExpTable[i] = byte(x)
		hembLogTable[byte(x)] = byte(i)
		hi := x << 1
		if hi >= 256 {
			hi ^= 0x11B
		}
		x = hi ^ x // multiply by generator 0x03 = (x+1)
	}
	for i := 0; i < 255; i++ {
		hembExpTable[i+255] = hembExpTable[i]
	}
}

func hembGFAdd(a, b byte) byte { return a ^ b }

func hembGFMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return hembExpTable[int(hembLogTable[a])+int(hembLogTable[b])]
}

func hembGFInv(a byte) byte {
	if a == 0 {
		panic("hembGFInv: zero has no inverse in GF(256)")
	}
	return hembExpTable[255-int(hembLogTable[a])]
}

type hembGFMatrix struct {
	Rows, Cols int
	Data       []byte
}

func newHeMBGFMatrix(rows, cols int) *hembGFMatrix {
	return &hembGFMatrix{Rows: rows, Cols: cols, Data: make([]byte, rows*cols)}
}

func (m *hembGFMatrix) get(row, col int) byte    { return m.Data[row*m.Cols+col] }
func (m *hembGFMatrix) set(row, col int, v byte) { m.Data[row*m.Cols+col] = v }

// ErrHeMBRankDeficient is returned when Gaussian elimination cannot find
// K linearly independent rows.
var ErrHeMBRankDeficient = errors.New("hemb: rank deficient — insufficient independent symbols")

// hembGaussianEliminate solves coeffs * X = payloads over GF(256).
func hembGaussianEliminate(coeffs *hembGFMatrix, payloads [][]byte) ([][]byte, error) {
	n := coeffs.Rows
	k := coeffs.Cols
	if n < k {
		return nil, fmt.Errorf("%w: have %d rows, need %d", ErrHeMBRankDeficient, n, k)
	}
	if len(payloads) != n {
		return nil, fmt.Errorf("hemb: payload count %d != row count %d", len(payloads), n)
	}
	if k == 0 {
		return nil, nil
	}

	payloadLen := len(payloads[0])
	for i := 1; i < n; i++ {
		if len(payloads[i]) != payloadLen {
			return nil, fmt.Errorf("hemb: payload %d length %d, expected %d", i, len(payloads[i]), payloadLen)
		}
	}

	mat := newHeMBGFMatrix(n, k)
	copy(mat.Data, coeffs.Data)
	pld := make([][]byte, n)
	for i := 0; i < n; i++ {
		pld[i] = make([]byte, payloadLen)
		copy(pld[i], payloads[i])
	}

	for col := 0; col < k; col++ {
		pivotRow := -1
		for row := col; row < n; row++ {
			if mat.get(row, col) != 0 {
				pivotRow = row
				break
			}
		}
		if pivotRow < 0 {
			return nil, fmt.Errorf("%w: column %d has no pivot", ErrHeMBRankDeficient, col)
		}

		if pivotRow != col {
			for c := 0; c < k; c++ {
				mat.Data[col*k+c], mat.Data[pivotRow*k+c] = mat.Data[pivotRow*k+c], mat.Data[col*k+c]
			}
			pld[col], pld[pivotRow] = pld[pivotRow], pld[col]
		}

		inv := hembGFInv(mat.get(col, col))
		for c := 0; c < k; c++ {
			mat.set(col, c, hembGFMul(mat.get(col, c), inv))
		}
		for j := 0; j < payloadLen; j++ {
			pld[col][j] = hembGFMul(pld[col][j], inv)
		}

		for row := 0; row < n; row++ {
			if row == col {
				continue
			}
			factor := mat.get(row, col)
			if factor == 0 {
				continue
			}
			for c := 0; c < k; c++ {
				mat.set(row, c, hembGFAdd(mat.get(row, c), hembGFMul(factor, mat.get(col, c))))
			}
			for j := 0; j < payloadLen; j++ {
				pld[row][j] = hembGFAdd(pld[row][j], hembGFMul(factor, pld[col][j]))
			}
		}
	}

	result := make([][]byte, k)
	for i := 0; i < k; i++ {
		result[i] = pld[i]
	}
	return result, nil
}
