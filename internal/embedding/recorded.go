// SPDX-License-Identifier: Apache-2.0

package embedding

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// The recorded embedder (feature 008 research R10).
//
// A fixture's vectors.jsonl holds the vectors one named model produced for every text the fixture
// embeds: its sections, as knowledge.EmbeddingText spells them, and its queries. CI replays them,
// so the hybrid path is gated on every pull request with no provider and no key, and a ranking is
// identical on every machine. An unknown text is refused, never approximated: a fixture whose
// sections changed must be re-recorded, and saying so is the whole point.
//
// The file's first line names the model; every other line is one text, keyed by its purpose and
// the SHA-256 of the text — never the text itself, so a recording carries nothing a section did
// not already — with its vector as little-endian float32, base64. That is exact and a third of the
// size of decimal JSON.

// VectorsFile is the file name a fixture's recorded vectors live in.
const VectorsFile = "vectors.jsonl"

// ErrNotRecorded is returned for a text the recording does not hold.
var ErrNotRecorded = errors.New("embedding: text not in the recording")

type vectorsHeader struct {
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Name       string `json:"name"`
	Dimensions int    `json:"dimensions"`
}

type vectorLine struct {
	Purpose knowledge.Purpose `json:"purpose"`
	SHA256  string            `json:"sha256"`
	Vector  string            `json:"vector"`
}

type recordKey struct {
	purpose knowledge.Purpose
	sha     string
}

// Recorded replays recorded vectors.
type Recorded struct {
	model   knowledge.ModelID
	vectors map[recordKey][]float32
}

var _ knowledge.Embedder = (*Recorded)(nil)

// LoadRecorded reads a vectors.jsonl file.
func LoadRecorded(path string) (*Recorded, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := ReadRecorded(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// ReadRecorded reads recorded vectors.
func ReadRecorded(r io.Reader) (*Recorded, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("embedding: recorded vectors: empty file; the first line names the model")
	}
	var header vectorsHeader
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil || header.Provider == "" || header.Name == "" || header.Dimensions <= 0 {
		return nil, fmt.Errorf("embedding: recorded vectors: the first line must name the model as {provider, name, dimensions}")
	}
	out := &Recorded{
		model:   knowledge.ModelID{Provider: header.Provider, Model: header.Name, Dimensions: header.Dimensions},
		vectors: map[recordKey][]float32{},
	}
	line := 1
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var v vectorLine
		if err := json.Unmarshal(scanner.Bytes(), &v); err != nil {
			return nil, fmt.Errorf("embedding: recorded vectors: line %d: %w", line, err)
		}
		vector, err := decodeVector(v.Vector)
		if err != nil {
			return nil, fmt.Errorf("embedding: recorded vectors: line %d: %w", line, err)
		}
		if len(vector) != header.Dimensions {
			return nil, fmt.Errorf("embedding: recorded vectors: line %d: %w: %d, the model has %d",
				line, knowledge.ErrDimensionMismatch, len(vector), header.Dimensions)
		}
		out.vectors[recordKey{v.Purpose, v.SHA256}] = vector
	}
	return out, scanner.Err()
}

// Model names the model the vectors were recorded from.
func (r *Recorded) Model() knowledge.ModelID { return r.model }

// Len is how many texts the recording holds.
func (r *Recorded) Len() int { return len(r.vectors) }

// Embed returns the recorded vector of every text, or ErrNotRecorded naming the first it lacks.
func (r *Recorded) Embed(_ context.Context, texts []string, purpose knowledge.Purpose) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		vector, ok := r.vectors[recordKey{purpose, textSHA(text)}]
		if !ok {
			return nil, fmt.Errorf("%w: a %s text of %d characters (sha256 %s); re-record the fixture's %s",
				ErrNotRecorded, purpose, len([]rune(text)), textSHA(text)[:12], VectorsFile)
		}
		out = append(out, append([]float32(nil), vector...))
	}
	return out, nil
}

// Recorder wraps a live embedder and keeps every vector it returns, to be written as a fixture's
// vectors.jsonl.
type Recorder struct {
	inner   knowledge.Embedder
	mu      sync.Mutex
	vectors map[recordKey][]float32
}

var _ knowledge.Embedder = (*Recorder)(nil)

// NewRecorder records what inner returns.
func NewRecorder(inner knowledge.Embedder) *Recorder {
	return &Recorder{inner: inner, vectors: map[recordKey][]float32{}}
}

// Model is the inner embedder's.
func (r *Recorder) Model() knowledge.ModelID { return r.inner.Model() }

// Embed embeds through the inner embedder, asking it only for texts not already recorded.
func (r *Recorder) Embed(ctx context.Context, texts []string, purpose knowledge.Purpose) ([][]float32, error) {
	var missing []string
	r.mu.Lock()
	for _, text := range texts {
		if _, ok := r.vectors[recordKey{purpose, textSHA(text)}]; !ok {
			missing = append(missing, text)
		}
	}
	r.mu.Unlock()
	if len(missing) > 0 {
		vectors, err := r.inner.Embed(ctx, missing, purpose)
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		for i, text := range missing {
			r.vectors[recordKey{purpose, textSHA(text)}] = vectors[i]
		}
		r.mu.Unlock()
	}
	out := make([][]float32, 0, len(texts))
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, text := range texts {
		out = append(out, append([]float32(nil), r.vectors[recordKey{purpose, textSHA(text)}]...))
	}
	return out, nil
}

// Write writes the recording as vectors.jsonl, lines sorted so a re-recording of the same texts
// by the same model is the same file.
func (r *Recorder) Write(w io.Writer) error {
	model := r.Model()
	header, err := json.Marshal(vectorsHeader{Model: model.String(), Provider: model.Provider, Name: model.Model, Dimensions: model.Dimensions})
	if err != nil {
		return err
	}
	r.mu.Lock()
	keys := make([]recordKey, 0, len(r.vectors))
	for k := range r.vectors {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].purpose != keys[j].purpose {
			return keys[i].purpose < keys[j].purpose
		}
		return keys[i].sha < keys[j].sha
	})
	lines := make([][]byte, 0, len(keys))
	for _, k := range keys {
		line, err := json.Marshal(vectorLine{Purpose: k.purpose, SHA256: k.sha, Vector: encodeVector(r.vectors[k])})
		if err != nil {
			r.mu.Unlock()
			return err
		}
		lines = append(lines, line)
	}
	r.mu.Unlock()
	bw := bufio.NewWriter(w)
	if _, err := bw.Write(append(header, '\n')); err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := bw.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// WriteFile writes the recording to path.
func (r *Recorder) WriteFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := r.Write(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func textSHA(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func encodeVector(v []float32) string {
	buf := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(x))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func decodeVector(s string) ([]float32, error) {
	buf, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("vector: %w", err)
	}
	if len(buf)%4 != 0 {
		return nil, errors.New("vector: length is not a whole number of float32")
	}
	out := make([]float32, len(buf)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[4*i:]))
	}
	return out, nil
}
