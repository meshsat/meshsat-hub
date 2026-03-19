// Package msvqsc implements the MSVQ-SC (Multi-Stage Vector Quantization
// Semantic Compression) decoder for MeshSat Hub. This is a pure-math decoder
// that reconstructs text from codebook indices — no ML runtime needed.
//
// Wire format: [1B header: stages(4bit)|version(4bit)][2B uint16 LE per stage]
// Decode: sum codebook vectors at each stage → cosine similarity → nearest corpus text.
package msvqsc

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

const (
	Version       = 1
	HeaderSize    = 1
	IndexSize     = 2 // uint16 LE per stage
	CodebookMagic = "MSVQ"
	CorpusMagic   = "MCIX"
)

// Decoder decodes MSVQ-SC wire payloads to text using codebook + corpus lookup.
type Decoder struct {
	version    int
	stages     int
	k          int
	dim        int
	vectors    [][][]float32 // [stage][entry][dim]
	corpus     []string
	corpusEmb  [][]float32 // [N][dim]
	corpusNorm []float32   // precomputed norms
}

// Load reads codebook and corpus index from files.
func Load(codebookPath, corpusPath string) (*Decoder, error) {
	cbData, err := os.ReadFile(codebookPath)
	if err != nil {
		return nil, fmt.Errorf("msvqsc: read codebook: %w", err)
	}
	ciData, err := os.ReadFile(corpusPath)
	if err != nil {
		return nil, fmt.Errorf("msvqsc: read corpus: %w", err)
	}
	return Parse(cbData, ciData)
}

// Parse creates a decoder from raw codebook and corpus data.
func Parse(codebookData, corpusData []byte) (*Decoder, error) {
	d := &Decoder{}

	// Parse codebook: "MSVQ" + version(1) + stages(1) + k(2 LE) + dim(2 LE) + vectors
	if len(codebookData) < 10 {
		return nil, fmt.Errorf("msvqsc: codebook too short (%d bytes)", len(codebookData))
	}
	if string(codebookData[:4]) != CodebookMagic {
		return nil, fmt.Errorf("msvqsc: invalid codebook magic: %q", string(codebookData[:4]))
	}

	d.version = int(codebookData[4])
	d.stages = int(codebookData[5])
	d.k = int(binary.LittleEndian.Uint16(codebookData[6:8]))
	d.dim = int(binary.LittleEndian.Uint16(codebookData[8:10]))

	expectedSize := 10 + d.stages*d.k*d.dim*4
	if len(codebookData) < expectedSize {
		return nil, fmt.Errorf("msvqsc: codebook truncated: need %d bytes, got %d", expectedSize, len(codebookData))
	}

	d.vectors = make([][][]float32, d.stages)
	offset := 10
	for s := 0; s < d.stages; s++ {
		d.vectors[s] = make([][]float32, d.k)
		for e := 0; e < d.k; e++ {
			d.vectors[s][e] = make([]float32, d.dim)
			for i := 0; i < d.dim; i++ {
				d.vectors[s][e][i] = math.Float32frombits(binary.LittleEndian.Uint32(codebookData[offset : offset+4]))
				offset += 4
			}
		}
	}

	// Parse corpus: "MCIX" + version(1) + numEntries(4 LE) + dim(2 LE) + entries
	if len(corpusData) < 11 {
		return nil, fmt.Errorf("msvqsc: corpus too short (%d bytes)", len(corpusData))
	}
	if string(corpusData[:4]) != CorpusMagic {
		return nil, fmt.Errorf("msvqsc: invalid corpus magic: %q", string(corpusData[:4]))
	}

	// Skip version byte
	numEntries := int(binary.LittleEndian.Uint32(corpusData[5:9]))
	corpusDim := int(binary.LittleEndian.Uint16(corpusData[9:11]))
	if corpusDim != d.dim {
		return nil, fmt.Errorf("msvqsc: corpus dim %d != codebook dim %d", corpusDim, d.dim)
	}

	d.corpus = make([]string, 0, numEntries)
	d.corpusEmb = make([][]float32, 0, numEntries)
	pos := 11

	for i := 0; i < numEntries; i++ {
		if pos+2 > len(corpusData) {
			break
		}
		textLen := int(binary.LittleEndian.Uint16(corpusData[pos : pos+2]))
		pos += 2

		if pos+textLen > len(corpusData) {
			break
		}
		text := string(corpusData[pos : pos+textLen])
		pos += textLen

		emb := make([]float32, d.dim)
		for j := 0; j < d.dim; j++ {
			if pos+4 > len(corpusData) {
				break
			}
			emb[j] = math.Float32frombits(binary.LittleEndian.Uint32(corpusData[pos : pos+4]))
			pos += 4
		}
		d.corpus = append(d.corpus, text)
		d.corpusEmb = append(d.corpusEmb, emb)
	}

	// Precompute corpus norms for faster cosine similarity.
	d.corpusNorm = make([]float32, len(d.corpusEmb))
	for i, emb := range d.corpusEmb {
		d.corpusNorm[i] = norm(emb)
	}

	return d, nil
}

// LooksLikeMSVQSC checks if raw bytes look like an MSVQ-SC wire payload.
func LooksLikeMSVQSC(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	header := data[0]
	stages := int((header >> 4) & 0x0F)
	version := int(header & 0x0F)
	if version != Version {
		return false
	}
	if stages < 1 || stages > 8 {
		return false
	}
	expectedLen := HeaderSize + stages*IndexSize
	return len(data) == expectedLen
}

// Decode decodes MSVQ-SC wire bytes to reconstructed text.
func (d *Decoder) Decode(wire []byte) (string, error) {
	if len(wire) < HeaderSize {
		return "", fmt.Errorf("msvqsc: wire too short")
	}

	header := wire[0]
	stages := int((header >> 4) & 0x0F)
	version := int(header & 0x0F)

	if version != Version {
		return "", fmt.Errorf("msvqsc: unsupported version %d", version)
	}

	expectedLen := HeaderSize + stages*IndexSize
	if len(wire) < expectedLen {
		return "", fmt.Errorf("msvqsc: wire too short: need %d, got %d", expectedLen, len(wire))
	}

	// Unpack indices.
	indices := make([]int, stages)
	for s := 0; s < stages; s++ {
		off := HeaderSize + s*IndexSize
		indices[s] = int(binary.LittleEndian.Uint16(wire[off : off+2]))
	}

	return d.DecodeIndices(indices)
}

// DecodeIndices decodes codebook indices to nearest corpus text.
func (d *Decoder) DecodeIndices(indices []int) (string, error) {
	if len(d.corpus) == 0 {
		return "", fmt.Errorf("msvqsc: no corpus loaded")
	}

	// Reconstruct embedding: sum codebook vectors at each stage.
	reconstructed := make([]float32, d.dim)
	for s := 0; s < len(indices) && s < d.stages; s++ {
		idx := indices[s]
		if idx < 0 || idx >= d.k {
			return "", fmt.Errorf("msvqsc: index %d out of range (K=%d) at stage %d", idx, d.k, s)
		}
		for i := 0; i < d.dim; i++ {
			reconstructed[i] += d.vectors[s][idx][i]
		}
	}

	// Find nearest corpus entry by cosine similarity.
	reconNorm := norm(reconstructed)
	bestIdx := 0
	var bestSim float32 = -1

	for i := range d.corpus {
		denom := reconNorm * d.corpusNorm[i]
		if denom < 1e-8 {
			continue
		}
		sim := dot(reconstructed, d.corpusEmb[i]) / denom
		if sim > bestSim {
			bestSim = sim
			bestIdx = i
		}
	}

	return d.corpus[bestIdx], nil
}

// Stats returns decoder metadata.
func (d *Decoder) Stats() (stages, k, dim, corpusSize int) {
	return d.stages, d.k, d.dim, len(d.corpus)
}

func dot(a, b []float32) float32 {
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

func norm(v []float32) float32 {
	return float32(math.Sqrt(float64(dot(v, v))))
}
